// bhyved
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/syslog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gitlab.com/stephen-fox/bhyved/internal/bhyver"
	"gitlab.com/stephen-fox/bhyved/internal/hsio"
	"gitlab.com/stephen-fox/bhyved/internal/lctx"
	"gitlab.com/stephen-fox/bhyved/internal/passfd"
	"gitlab.com/stephen-fox/bhyved/internal/rwsrv"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const (
	appName = "bhyved"
	usage   = appName + `

SYNOPSIS
  ` + appName + ` [options] genmac [genmac-options]
  ` + appName + ` [options] daemon [daemon-options] -- <bhyve-args>
  ` + appName + ` [options] power [power-options] <vm-name> <on|off|pull-cable|reboot|pull-cable-reboot>
  ` + appName + ` [options] console [console-options] <vm-name>

DESCRIPTION

(TODO ...)

OPTIONS
`

	vmRuntimeDirPerm = fs.FileMode(0o755)
	vmSocketsPerm    = fs.FileMode(0o660)
)

func main() {
	log.SetFlags(0)

	err := mainWithError()
	if err != nil {
		log.Fatalln("fatal:", err)
	}
}

func mainWithError() error {
	displayHelp := flag.Bool("h", false, "Display this information")

	flag.Parse()

	if *displayHelp {
		_, _ = os.Stderr.WriteString(usage)
		flag.PrintDefaults()
		os.Exit(1)
	}

	if flag.NArg() == 0 {
		return errors.New("please specify a mode as a non-flag argument or '-h' for more information")
	}

	flagSet := flag.NewFlagSet(flag.Arg(0), flag.ExitOnError)

	switch flag.Arg(0) {
	case "genmac":
		return genmac()
	case "daemon":
		return daemon(flagSet)
	case "console-daemon":
		return consoleDaemon(flagSet)
	case "power":
		return power(flagSet)
	case "console":
		return console(flagSet)
	default:
		return fmt.Errorf("unknown mode: '%s'", flag.Arg(0))
	}
}

func genmac() error {
	var retriesRemaining int

	var mac string
	b := make([]byte, 1)

	for i := 0; i < 6; i++ {
		retriesRemaining = 5

	retry:
		if retriesRemaining == 0 {
			return errors.New("read 0 after 5 retries")
		}

		_, err := rand.Read(b)
		if err != nil {
			return err
		}

		if b[0] == 0x00 {
			retriesRemaining--
			goto retry
		}

		if i == 0 {
			// Last two bits need to be:
			// 1 0
			b[0] <<= 2
			b[0] ^= 0b00000010
		}

		mac += hex.EncodeToString(b)
		if i != 5 {
			mac += ":"
		}
	}

	os.Stdout.WriteString(mac + "\n")

	return nil
}

func daemon(flagSet *flag.FlagSet) error {
	foreground := flagSet.Bool(
		"F",
		false,
		"Stay in the foreground rather than exec'ing into background")

	enableConsoleDaemon := flagSet.Bool(
		"C",
		false,
		"Enable serial console access using the 'console' mode\n"+
			"(requires that bhyve use the serial console 'stdio' mode - e.g.,\n"+
			"'-s 31,lpc -l com1,stdio')")

	startSyslogd := flagSet.Bool(
		"s",
		false,
		"Start syslogd prior to executing bhyve")

	runAsUser := flagSet.String(
		"u",
		"",
		"Optionally run as a specific user (only supported on Unix systems)")

	pidFilePath := flagSet.String(
		"p",
		"/var/run/"+appName+".pid",
		"Create a PID file at this file path (specify '-' to disable)\n")

	_ = flagSet.Parse(os.Args[2:])

	if flagSet.NArg() == 0 {
		return errors.New("please specify bhyve arguments after '--'")
	}

	var err error
	flagSet.VisitAll(func(f *flag.Flag) {
		if err != nil {
			return
		}

		if strings.Contains(strings.ToLower(f.Usage), "optional") {
			return
		}

		if f.Value.String() == "" {
			err = fmt.Errorf("please specify '-%s' - %s", f.Name, f.Usage)
		}
	})
	if err != nil {
		return err
	}

	if !path.IsAbs(os.Args[0]) {
		return fmt.Errorf("application path must be absolute - '%s' is a relative path",
			os.Args[0])
	}

	if flagSet.NArg() == 0 {
		return errors.New("please specify at least one bhyve argument after --")
	}

	const childEnvName = appName + "_" + "child"

	isChild := os.Getenv(childEnvName) != "" || *foreground
	if !isChild {
		if *startSyslogd {
			syslogd := exec.Command("/usr/sbin/syslogd", "-s")
			syslogd.SysProcAttr = &syscall.SysProcAttr{
				Setpgid: true, // Do not propogate signals to child.
			}

			// Ignore the error because syslogd may already be running.
			_ = syslogd.Run()

			start := time.Now()

			for {
				if time.Since(start) > 5*time.Second {
					return errors.New("timed-out waiting for syslogd to start")
				}

				writer, err := syslog.New(syslog.LOG_DAEMON, "")
				if err != nil {
					time.Sleep(100 * time.Millisecond)
					continue
				}

				writer.Close()

				break
			}
		}

		var childSysProcAttr *syscall.SysProcAttr
		if *runAsUser != "" {
			// TODO: Re-implement this.
			return errors.New("TODO: this needs to be re-implemented :(")
		}

		restarted := exec.Command(os.Args[0], os.Args[1:]...)

		stdoutPipe, err := restarted.StdoutPipe()
		if err != nil {
			return fmt.Errorf("failed to create stdout pipe for child - %w", err)
		}
		defer stdoutPipe.Close()

		gotWrite := make(chan error, 1)
		go func() {
			_, err := stdoutPipe.Read(make([]byte, 1))
			gotWrite <- err
		}()

		restarted.Stderr = os.Stderr
		restarted.Dir = "/var/empty"
		restarted.Env = os.Environ()
		restarted.Env = append(restarted.Env, childEnvName+"=true")
		restarted.SysProcAttr = childSysProcAttr

		err = restarted.Start()
		if err != nil {
			return fmt.Errorf("failed to exec to background - %w", err)
		}

		if *pidFilePath != "-" {
			_ = os.Remove(*pidFilePath)

			err = os.WriteFile(
				*pidFilePath,
				[]byte(fmt.Sprintf("%d\n", restarted.Process.Pid)),
				0o644)
			if err != nil {
				_ = restarted.Process.Kill()
				return fmt.Errorf("failed to write pid file '%s' - %w",
					*pidFilePath, err)
			}
		}

		select {
		case <-time.After(5 * time.Second):
			_ = restarted.Process.Kill()

			return errors.New("timed-out waiting for child process to be ready")
		case err = <-gotWrite:
			if err != nil {
				_ = restarted.Process.Kill()

				return fmt.Errorf("failed to read from child's stdout - %w", err)
			}

			return nil
		}
	}

	vmName := flagSet.Arg(flagSet.NArg() - 1)

	if *foreground {
		log.SetFlags(log.LstdFlags)
	} else {
		syslogWriter, err := syslog.New(syslog.LOG_DAEMON, appName+" - "+vmName)
		if err != nil {
			return fmt.Errorf("failed to open syslog - %w", err)
		}

		log.SetOutput(syslogWriter)
	}

	vmDirPath := vmRuntimeDirPath(vmName)

	err = os.MkdirAll(vmDirPath, vmRuntimeDirPerm)
	if err != nil {
		return fmt.Errorf("failed to create vm dir path - %w", err)
	}

	err = os.Chmod(vmDirPath, vmRuntimeDirPerm)
	if err != nil {
		return fmt.Errorf("failed to chmod vm dir path %q - %w", vmDirPath, err)
	}

	ctx, cancelFn := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	defer cancelFn()

	powerStateListener, err := lctx.ListenUnixPath(
		ctx,
		powerStateSocketPath(vmDirPath),
		vmSocketsPerm)
	if err != nil {
		return fmt.Errorf("failed to create power state unix socket - %w", err)
	}
	defer powerStateListener.Close()

	powerStateRequests := bhyver.PowerStateRequestsHanlder(ctx, powerStateListener)

	var optConsoleDaemon *consoleDaemonChild
	var optConsoleDaemonDone <-chan struct{}
	var optConsoleDaemonConn *net.UnixConn

	if *enableConsoleDaemon {
		log.Println("setting up console daemon...")

		optConsoleDaemon, err = execConsoleDaemon(ctx, vmDirPath)
		if err != nil {
			return fmt.Errorf("failed to start console daemon - %w", err)
		}
		defer optConsoleDaemon.Kill()

		optConsoleDaemonDone = optConsoleDaemon.Done()
		optConsoleDaemonConn = optConsoleDaemon.FdConn()

		log.Println("console daemon started successfully")
	}

	// TOOD: Send bhyve stderr to syslog.
	runner := bhyver.StartRunner(
		ctx,
		vmName,
		flagSet.Args(),
		powerStateRequests,
		optConsoleDaemonConn)

	// Wait to see if runner exits due to a bhyve error.
	select {
	case <-runner.Done():
		// TODO: If jail scenarios, it appears that syslogd
		// does not get all the writes we sent it if we exit
		// quickly. Need to investigate by forcing a bhyve
		// misconfiguration.
		return fmt.Errorf("bhyve runner exited unexpectedly during initial start - %w",
			runner.Err())
	case <-time.After(2 * time.Second):
		if !*foreground {
			os.Stdout.Write([]byte{0x41})
		}
	}

	select {
	case <-runner.Done():
		return fmt.Errorf("bhyve runner exited - %w", runner.Err())
	case <-optConsoleDaemonDone:
		err = fmt.Errorf("console daemon exited unexpectedly - shutting down (err: %w)",
			optConsoleDaemon.Err())

		log.Println(err)

		cancelFn()

		<-runner.Done()

		return err
	}
}

type consoleDaemonChild struct {
	fdsConn *net.UnixConn
	execCmd *exec.Cmd
	done    chan struct{}
	err     error
}

func (o *consoleDaemonChild) Done() <-chan struct{} {
	return o.done
}

func (o *consoleDaemonChild) Err() error {
	return o.err
}

func (o *consoleDaemonChild) FdConn() *net.UnixConn {
	return o.fdsConn
}

func (o *consoleDaemonChild) Kill() error {
	return o.execCmd.Process.Kill()
}

func execConsoleDaemon(ctx context.Context, vmDirPath string) (*consoleDaemonChild, error) {
	runAsUID, runAsGID, err := lookupUser("nobody")
	if err != nil {
		return nil, fmt.Errorf("failed to lookup run as user - %w", err)
	}

	daemonLog, err := os.OpenFile(
		filepath.Join(vmDirPath, "console-daemon.log"),
		os.O_CREATE|os.O_TRUNC|os.O_WRONLY,
		0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to open console daemon log file - %w", err)
	}
	defer daemonLog.Close()

	// TODO: Should we truncate the log?
	consoleLog, err := os.OpenFile(
		filepath.Join(vmDirPath, "console-output.log"),
		os.O_CREATE|os.O_TRUNC|os.O_WRONLY,
		0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to open vm console output log file - %w", err)
	}
	defer consoleLog.Close()

	clientsLn, clientsLnFd, err := passfd.ShareableUnixListener(
		consoleSocketPath(vmDirPath),
		vmSocketsPerm)
	if err != nil {
		return nil, fmt.Errorf("failed to create console client unix socket - %w", err)
	}
	defer func() {
		clientsLnFd.Close()
		if err != nil {
			clientsLn.Close()
		}
	}()

	ourConsoleFdSocket, theirConsoleFdSocket, err := passfd.ShareableUnixSocketpair(
		syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("sharable socketpair failed - %w", err)
	}
	defer func() {
		theirConsoleFdSocket.Close()
		if err != nil {
			ourConsoleFdSocket.Close()
		}
	}()

	consoleDaemon := exec.Command(os.Args[0], "console-daemon")

	stdoutPipe, err := consoleDaemon.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe - %w", err)
	}
	defer stdoutPipe.Close()

	stderrPipe, err := consoleDaemon.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stderr pipe - %w", err)
	}
	defer stderrPipe.Close()

	stderr := bytes.NewBuffer(nil)
	go io.Copy(stderr, stderrPipe)

	consoleDaemon.Env = []string{}

	consoleDaemon.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: runAsUID,
			Gid: runAsGID,
		},
		// Do not propogate signals sent to us to child.
		Setpgid: true,
	}

	consoleDaemon.ExtraFiles = []*os.File{
		daemonLog,
		consoleLog,
		clientsLnFd,
		theirConsoleFdSocket,
	}

	err = consoleDaemon.Start()
	if err != nil {
		return nil, fmt.Errorf("failed to start child process - %w", err)
	}

	gotWrite := make(chan error, 1)

	go func() {
		_, err := stdoutPipe.Read(make([]byte, 1))
		gotWrite <- err
	}()

	timeout := 10 * time.Second

	select {
	case <-ctx.Done():
		_ = consoleDaemon.Process.Kill()

		return nil, ctx.Err()
	case <-time.After(timeout):
		_ = consoleDaemon.Process.Kill()

		return nil, fmt.Errorf("timed-out waiting for write from child after %s", timeout)
	case err = <-gotWrite:
		if err != nil {
			_ = consoleDaemon.Process.Kill()
			_ = stderrPipe.Close() // Ensures stderr buffer writes are done.

			return nil, fmt.Errorf("failed to receive ready write from child - %w - stderr: %s",
				err, stderr.String())
		}

		child := &consoleDaemonChild{
			fdsConn: ourConsoleFdSocket,
			execCmd: consoleDaemon,
			done:    make(chan struct{}),
		}

		go func() {
			child.err = consoleDaemon.Wait()

			clientsLn.Close()
			ourConsoleFdSocket.Close()

			close(child.done)
		}()

		return child, nil
	}
}

func lookupUser(username string) (uid uint32, gid uint32, err error) {
	account, err := user.Lookup(username)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to lookup user '%s' - %w", username, err)
	}

	uidI, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to parse uid - %w", err)
	}

	gidI, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to parse gid - %w", err)
	}

	return uint32(uidI), uint32(gidI), nil
}

func consoleDaemon(flagSet *flag.FlagSet) error {
	_ = flagSet.Parse(os.Args[2:])

	ctx, cancelFn := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer cancelFn()

	daemonLogFile := os.NewFile(3, "")
	if daemonLogFile == nil {
		return errors.New("os new file returned nil for the daemon log file - invalid fd")
	}
	defer daemonLogFile.Close()

	consoleLogFile := os.NewFile(4, "")
	if consoleLogFile == nil {
		return errors.New("os new file returned nil for the console log file - invalid fd")
	}
	defer consoleLogFile.Close()

	consoleClientsListener, err := lctx.FromFileDescriptor(ctx, 5, "")
	if err != nil {
		return fmt.Errorf("failed to convert console clients fd to a listener - %w", err)
	}
	defer consoleClientsListener.Close()

	consoleFdsConn, err := passfd.UnixConnFromFd(6, "")
	if err != nil {
		return fmt.Errorf("failed to convert console sharing fd to a unix conn - %w", err)
	}
	defer consoleFdsConn.Close()

	_, err = os.Stdout.Write([]byte{0x41})
	if err != nil {
		return fmt.Errorf("failed to write ready message to stdout - %w", err)
	}

	log.SetFlags(log.LstdFlags)
	log.SetOutput(daemonLogFile)

	consoleStdin := hsio.NewWriteCloser()
	consoleStdout := hsio.NewReadCloser()

	passfdClient := passfd.NewClient(ctx, consoleFdsConn, []func(*os.File) error{
		passfd.WriteCloserUpdaterToRecvFn(consoleStdin),
		passfd.ReadCloserUpdaterToRecvFn(consoleStdout),
	})

	rwServer := rwsrv.New(ctx, rwsrv.Config{
		Listener:  consoleClientsListener,
		Src:       consoleStdout,
		Dst:       consoleStdin,
		OptBufSz:  32 * 1024,
		OptSrcLog: consoleLogFile,
	})

	select {
	case <-ctx.Done():
		err = ctx.Err()
	case <-passfdClient.Done():
		err = fmt.Errorf("passfd client exited - %w", passfdClient.Err())
	case <-rwServer.Done():
		err = fmt.Errorf("rw server exited - %w", rwServer.Err())
	}

	cancelFn()

	<-passfdClient.Done()
	<-rwServer.Done()

	return err
}

func power(flagSet *flag.FlagSet) error {
	// doNotFailIfDaemonIsStopped := flagSet.Bool(
	// 	"",
	// 	false,
	// 	"")

	singleVmMode := flagSet.Bool(
		"M",
		false,
		"Automatically pick the vm (only permitted if one vm is running)")

	_ = flagSet.Parse(os.Args[2:])

	vmName := flagSet.Arg(0)
	if vmName == "" {
		if !*singleVmMode {
			return errors.New("please specify a vm name as the first non-flag argument")
		}

		entries, err := os.ReadDir("/dev/vmm")
		if err != nil {
			return fmt.Errorf("failed to read vmm dir - %w", err)
		}

		if len(entries) != 1 {
			return fmt.Errorf("expected only one vm to be running - found: %d",
				len(entries))
		}

		vmName = entries[0].Name()
	}

	vmDirPath := vmRuntimeDirPath(vmName)

	powerStateStr := flagSet.Arg(1)
	if powerStateStr == "" {
		return errors.New("please specify a power state as the last non-flag argument")
	}

	state := bhyver.PowerStateFromString(powerStateStr)
	if state == bhyver.UnknownPowerState {
		return fmt.Errorf("unknown power state type: %q", powerStateStr)
	}

	conn, err := net.Dial("unix", powerStateSocketPath(vmDirPath))
	if err != nil {
		return fmt.Errorf("failed to open power state unix socket - %w", err)
	}
	defer conn.Close()

	_, err = conn.Write([]byte(state.String() + "\n"))
	if err != nil {
		return err
	}

	scanner := bufio.NewScanner(conn)

	if !scanner.Scan() {
		return scanner.Err()
	}

	errMsg := scanner.Text()
	if errMsg != "" {
		return errors.New(errMsg)
	}

	return nil
}

func console(flagSet *flag.FlagSet) error {
	singleVmMode := flagSet.Bool(
		"M",
		false,
		"Automatically pick the vm (only permitted if one vm is running)")

	allowStdinToClose := flagSet.Bool(
		"w",
		false,
		"Do not exit if stdin is closed (useful for writing to stdin in a shell,\n"+
			"closing it, and waiting until the daemon shuts down)")

	_ = flagSet.Parse(os.Args[2:])

	vmName := flagSet.Arg(0)
	if vmName == "" {
		if !*singleVmMode {
			return errors.New("please specify a vm name as the first non-flag argument")
		}

		entries, err := os.ReadDir("/dev/vmm")
		if err != nil {
			return fmt.Errorf("failed to read vmm dir - %w", err)
		}

		if len(entries) != 1 {
			return fmt.Errorf("expected only one vm to be running - found: %d",
				len(entries))
		}

		vmName = entries[0].Name()
	}

	vmDirPath := vmRuntimeDirPath(vmName)

	conn, err := net.Dial("unix", consoleSocketPath(vmDirPath))
	if err != nil {
		return fmt.Errorf("failed to open console unix socket - %w", err)
	}
	defer conn.Close()

	if os.Getuid() == 0 {
		runAsUID, runAsGID, err := lookupUser("nobody")
		if err != nil {
			return fmt.Errorf("failed to lookup nobody user - %w", err)
		}

		chrootDirPath := "/var/empty"
		err = syscall.Chroot(chrootDirPath)
		if err != nil {
			return fmt.Errorf("failed to chroot to %q - %w",
				chrootDirPath, err)
		}

		err = dropPrivsToUser(int(runAsUID), int(runAsGID))
		if err != nil {
			return fmt.Errorf("failed to drop privs to uid %d gid %d - %w",
				runAsUID, runAsGID, err)
		}
	}

	previousTermState, err := term.MakeRaw(0)
	if err != nil {
		return fmt.Errorf("failed to put terminal into raw mode - %w", err)
	}
	defer term.Restore(0, previousTermState)

	err = unix.CapEnter()
	if err != nil {
		return fmt.Errorf("failed to enter capability mode - %w", err)
	}

	errs := make(chan error, 2)

	go func() {
		err := copyStdinToConsole(conn)
		if err != nil {
			errs <- err
			return
		}

		if !*allowStdinToClose {
			errs <- nil
		}
	}()

	go func() {
		_, err := io.Copy(os.Stdout, conn)
		errs <- err
	}()

	// I prefer seeing the exact reason compared to "context canceled".
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	defer signal.Stop(signals)

	select {
	case s := <-signals:
		return fmt.Errorf("received signal: %s / %d", s.String(), s)
	case err = <-errs:
		return err
	}
}

func dropPrivsToUser(uid int, gid int) error {
	err := unix.Setresgid(gid, gid, gid)
	if err != nil {
		return fmt.Errorf("setresgid failed for gid %d - %w", gid, err)
	}

	err = unix.Setresuid(uid, uid, uid)
	if err != nil {
		return fmt.Errorf("setresuid failed for uid %d - %w", uid, err)
	}

	return nil
}

func copyStdinToConsole(conn io.Writer) error {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Split(bufio.ScanBytes)

	// From logging: 0xd 0d 7e 2e
	// \n~.
	// Apparently, \n is 0x09.
	const stopTerminalEscSeq uint32 = 0x0d_7e_2e_00
	const startOfEscSeq uint32 = 0x0d_7e_00_00

	var lastFourBytes uint32
	var escSeqStarted bool

	for scanner.Scan() {
		b := scanner.Bytes()[0]

		// 0xaa41bb42 becomes 0x41bb4200.
		lastFourBytes <<= 8
		// 0x41bb4200 becomes 0x41bb42ff where ff is the new byte.
		lastFourBytes |= uint32(b)

		switch {
		case (lastFourBytes<<16)^startOfEscSeq == 0:
			// Do not send the "~" until the user sends the next byte.
			escSeqStarted = true
			continue
		case (lastFourBytes<<8)^stopTerminalEscSeq == 0:
			return nil
		case escSeqStarted:
			// The next byte was not ".", send "~".
			escSeqStarted = false

			_, err := conn.Write([]byte{0x7e})
			if err != nil {
				return fmt.Errorf("failed to write to conn - %w", err)
			}
		}

		_, err := conn.Write(scanner.Bytes())
		if err != nil {
			return fmt.Errorf("failed to write to conn - %w", err)
		}
	}

	return scanner.Err()
}

func consoleSocketPath(runtimeDirPath string) string {
	return filepath.Join(runtimeDirPath, "console.sock")
}

func powerStateSocketPath(runtimeDirPath string) string {
	return filepath.Join(runtimeDirPath, "power.sock")
}

func vmRuntimeDirPath(vmName string) string {
	return filepath.Join(appRuntimeDirPath(), vmName)
}

func appRuntimeDirPath() string {
	return filepath.Join("/var", appName)
}

type fileModeFlag struct {
	mode os.FileMode
}

func (o *fileModeFlag) String() string {
	return fmt.Sprintf("%o (%s)", o.mode, o.mode.String())
}

func (o *fileModeFlag) Set(s string) error {
	i, err := strconv.ParseInt(s, 8, 32)
	if err != nil {
		return err
	}
	o.mode = os.FileMode(i)
	return nil
}
