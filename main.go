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
	"log"
	"log/syslog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
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
)

const (
	noClientFlag uint8 = iota
	bufferedOutputClientFlag
)

func main() {
	log.SetFlags(0)

	err := mainWithError()
	if err != nil {
		log.Fatalln(err)
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
		return errors.New("please specify a mode ('client' or 'daemon') or '-h' for more information")
	}

	flagSet := flag.NewFlagSet(flag.Arg(0), flag.ExitOnError)

	switch flag.Arg(0) {
	case "genmac":
		return genmac()
	case "daemon":
		return daemon(flagSet)
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
			// Set LSB 0 to 0.
			b[0] &= 0b11111110
			//b[0] <<= 1
			//b[0] <<= 0
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

	socketMode := fileModeFlag{
		mode: 0600,
	}
	flagSet.Var(
		&socketMode,
		"m",
		"The socket's file mode")

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
		"",
		"Optionally create a PID file at this file path")

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

			// Ignore the error because syslogd may already be running.
			_ = syslogd.Start()
		}

		var childSysProcAttr *syscall.SysProcAttr
		if *runAsUser != "" {
			// TODO: Re-implement this.
		}

		restarted := exec.Command(os.Args[0], os.Args[1:]...)
		restarted.Dir = "/var/empty"
		restarted.Env = os.Environ()
		restarted.Env = append(restarted.Env, childEnvName+"=true")
		restarted.SysProcAttr = childSysProcAttr

		err := restarted.Start()
		if err != nil {
			return fmt.Errorf("failed to exec to background - %w", err)
		}

		if *pidFilePath != "" {
			_ = os.Remove(*pidFilePath)

			err = os.WriteFile(
				*pidFilePath,
				[]byte(fmt.Sprintf("%d\n", restarted.Process.Pid)),
				0644)
			if err != nil {
				_ = restarted.Process.Kill()
				return fmt.Errorf("failed to write pid file '%s' - %w",
					*pidFilePath, err)
			}
		}

		return nil
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

	vmDirPath := dataDirPath(vmName)

	err = os.MkdirAll(vmDirPath, 0o755)
	if err != nil {
		return fmt.Errorf("failed to create vm data directory path - %w", err)
	}

	consoleOutputLog, err := os.OpenFile(
		filepath.Join(vmDirPath, "console-output.log"),
		os.O_CREATE|os.O_TRUNC|os.O_WRONLY,
		0o600)
	if err != nil {
		return fmt.Errorf("failed to open vm console output log file - %w", err)
	}
	defer consoleOutputLog.Close()

	ctx, cancelFn := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	defer cancelFn()

	powerStateAccepts, powerStateListener, err := newUnixSocket(
		ctx,
		powerStateSocketPath(vmName),
		socketMode.mode)
	if err != nil {
		return fmt.Errorf("failed to create power state unix socket - %w", err)
	}
	defer powerStateListener.Close()

	powerStateRequests := powerStateRequestsHanlder(ctx, powerStateAccepts)

	consoleAccepts, consoleListener, err := newUnixSocket(
		ctx,
		consoleSocketPath(vmName),
		socketMode.mode)
	if err != nil {
		return fmt.Errorf("failed to create console unix socket - %w", err)
	}
	defer consoleListener.Close()

	pipeReader, pipeWriter := io.Pipe()

	consoleManager := newConnManager(ctx, connManagerConfig{
		accepts:  consoleAccepts,
		logFile:  consoleOutputLog,
		connDest: pipeWriter,
	})
	defer consoleManager.close()

	bm := newBhyveManager(vmName, flagSet.Args(), pipeReader, consoleManager, powerStateRequests)

	return bm.loop(ctx)
}

func consoleSocketPath(vmName string) string {
	return filepath.Join(dataDirPath(vmName), "console.sock")
}

func powerStateSocketPath(vmName string) string {
	return filepath.Join(dataDirPath(vmName), "power.sock")
}

func dataDirPath(vmName string) string {
	return filepath.Join("/var", vmName)
}

func newBhyveManager(vmName string, bhyveArgs []string, stdin io.Reader, stdout io.Writer, powerRequests <-chan powerStateRequest) *bhyveManager {
	return &bhyveManager{
		vmName:    vmName,
		bhyveArgs: bhyveArgs,
		stdin:     stdin,
		stdout:    stdout,
		powerReqs: powerRequests,
		exited:    make(chan error, 1),
		stderr:    bytes.NewBuffer(nil),
	}
}

type bhyveManager struct {
	vmName    string
	bhyveArgs []string
	stdin     io.Reader
	stdout    io.Writer
	powerReqs <-chan powerStateRequest
	exited    chan error
	execCmd   *exec.Cmd
	stderr    *bytes.Buffer
}

func (o *bhyveManager) loop(ctx context.Context) error {
	err := o.start(ctx)
	if err != nil {
		return fmt.Errorf("failed to start bhyve for the first time - %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			timeout := time.Minute

			log.Printf("shutting down due to %s - waiting %s for bhyve to exit...",
				ctx.Err(), timeout.String())

			stopCtx, cancelFn := context.WithTimeout(context.Background(), timeout)
			defer cancelFn()

			err := o.acpiOffOrKill(stopCtx)
			if err != nil {
				log.Printf("failed to stop bhyve on shutdown - %s", err)
			} else {
				log.Println("successfully stopped bhyve")
			}

			return ctx.Err()
		case powerRequest := <-o.powerReqs:
			clientMsg, err := o.onPowerStateRequest(ctx, powerRequest.newState)

			if clientMsg != "" {
				powerRequest.cb <- errors.New(clientMsg)
			} else {
				powerRequest.cb <- nil
			}
			close(powerRequest.cb)

			if err != nil {
				return fmt.Errorf("failed to handle power state change - %w", err)
			}
		case exitedErr := <-o.exited:
			err := o.onExecCmdExit(ctx, exitedErr)
			if err != nil {
				return fmt.Errorf("failed to handle bhyve exit error - %w", err)
			}
		}
	}
}

func (o *bhyveManager) onPowerStateRequest(ctx context.Context, newState powerState) (clientMsg string, err error) {
	log.Printf("received power state request - new state: %q", newState.String())

	switch newState {
	case onPowerState:
		err := o.start(ctx)
		if err != nil {
			return err.Error(), fmt.Errorf("failed to start vm - %w", err)
		}

		return "", nil
	case acpiOffPowerState:
		err := o.acpiOffOrKill(ctx)
		if err != nil {
			return err.Error(), fmt.Errorf("failed to acpi power off - %w", err)
		}

		return "", nil
	case acpiRebootPowerState:
		err := o.acpiOffOrKill(ctx)
		if err != nil {
			return err.Error(), fmt.Errorf("failed to acpi power off for reboot - %w", err)
		}

		err = o.start(ctx)
		if err != nil {
			return err.Error(), fmt.Errorf("failed to start for reboot - %w", err)
		}

		return "", nil
	case pullPowerCablePowerState:
		err := o.pullPowerCable(ctx)
		if err != nil {
			return err.Error(), fmt.Errorf("failed to pull power cable - %w", err)
		}

		return "", nil
	case pullPowerCableRebootPowerState:
		err := o.pullPowerCable(ctx)
		if err != nil {
			return err.Error(), fmt.Errorf("failed to pull power cable for reboot - %w", err)
		}

		err = o.start(ctx)
		if err != nil {
			return err.Error(), fmt.Errorf("failed to start for pull power cable reboot - %w", err)
		}

		return "", nil
	default:
		log.Println("[warn] unknown power state type was requested")

		return "unknown power state type", nil
	}
}

func (o *bhyveManager) start(ctx context.Context) error {
	if o.isRunning() {
		log.Printf("[warn] - bhyve start was attempted, but process is already running")

		return nil
	}

	log.Println("starting bhyve...")

	// TODO: Check if the VM exists first.
	o.bhyvectl(ctx, "destroy")

	o.stderr.Reset()

	bhyve := exec.Command("/usr/sbin/bhyve", o.bhyveArgs...)

	bhyve.Stdin = o.stdin
	bhyve.SysProcAttr = &syscall.SysProcAttr{
		// We set Setpgid to true because, by default,
		// a signal sent to us will be automatically
		// sent to any children (i.e., pressing ctrl+c
		// to send SIGINT to bhyved will also send
		// a SIGINT to the bhyve child process).
		// This is bad because SIGINT makes bhyve
		// exit immediately.
		//
		// Setting this to true assigns bhyve to a new
		// process group ID, which will not receive
		// signals sent to the parent process.
		Setpgid: true,
	}

	bhyve.Stderr = o.stderr

	bhyve.Stdout = o.stdout

	log.Printf("starting bhyve (argv: '%s')...", bhyve.String())

	err := bhyve.Start()
	if err != nil {
		return fmt.Errorf("failed to start bhyve - %w", err)
	}

	o.execCmd = bhyve

	go func() {
		err := bhyve.Wait()

		log.Printf("bhyve exited - exec.cmd error is %v", err)

		o.exited <- err
	}()

	return nil
}

func (o *bhyveManager) acpiOffOrKill(ctx context.Context) error {
	if !o.isRunning() {
		log.Println("[warn] acpi off requested, but bhyve is not running")

		return nil
	}

	log.Println("acpi powering off or killing bhyve...")

	// Trigger ACPI poweroff, refer to "man bhyve" for more info.
	err := o.execCmd.Process.Signal(syscall.SIGTERM)
	if err != nil {
		log.Printf("failed to send sigterm to bhyve - %s", err)
	}

	select {
	case <-ctx.Done():
		pullPowerCtx, cancelFn := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelFn()

		_ = o.pullPowerCable(pullPowerCtx)

		return ctx.Err()
	case err = <-o.exited:
		log.Printf("bhyve process exited after sending sigterm - destroying vm with bhyvectl... (child err: %v)", err)

		err = o.bhyvectl(ctx, "--destroy")
		if err != nil {
			log.Printf("failed to destroy vm after stopping it - %s", err)
		}

		return nil
	}
}

func (o *bhyveManager) pullPowerCable(ctx context.Context) error {
	if !o.isRunning() {
		log.Println("[warn] pull power cable requested, but bhyve is not running")

		return nil
	}

	log.Println("pulling power cable from bhyve...")

	defer func() {
		log.Println("destroying vm with bhyvectl...")

		err := o.bhyvectl(ctx, "--destroy")
		if err != nil {
			log.Printf("failed to destroy vm after stopping it - %s", err)
		}
	}()

	_ = o.execCmd.Process.Signal(syscall.SIGKILL)

	select {
	case <-ctx.Done():
		log.Printf("timed-out waiting for bhyve to exit after sending sigkill - %s",
			ctx.Err())

		return ctx.Err()
	case <-o.exited:
		log.Println("bhyve exited after sending sigkill")

		return nil
	}
}

func (o *bhyveManager) bhyvectl(ctx context.Context, arg string, args ...string) error {
	bhyvectl := exec.CommandContext(
		ctx,
		"/usr/sbin/bhyvectl",
		"--vm",
		o.vmName,
		arg)

	if len(args) > 0 {
		bhyvectl.Args = append(bhyvectl.Args, args...)
	}

	out, err := bhyvectl.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to execute %q - %w - output: %s",
			bhyvectl.String(), err, out)
	}

	return nil
}

func (o *bhyveManager) isRunning() bool {
	// Exec.Cmd.ProcessState is non-nil if the process has exited.
	return o.execCmd != nil && o.execCmd.ProcessState == nil
}

func (o *bhyveManager) onExecCmdExit(ctx context.Context, exitedErr error) error {
	// Note: Refer to "man bhyve" for exit status info.
	if exitedErr == nil {
		// err == nil means exit status 0.
		log.Println("bhyve exited with status 0 - vm was rebooted")

		exitedErr = o.start(ctx)
		if exitedErr != nil {
			return fmt.Errorf("failed to start bhyve for vm reboot - %w", exitedErr)
		}

		return nil
	}

	var execExitErr *exec.ExitError
	if errors.As(exitedErr, &execExitErr) {
		switch execExitErr.ExitCode() {
		case 1:
			// Powered off.
			log.Println("bhyve exited with status 1 - vm was powered off")
		case 2:
			// Halted.
			log.Println("bhyve exited with status 2 - vm was halted")
		case 3:
			// Triple fault.
			log.Println("bhyve exited with status 3 - vm triple faulted")
		case 4:
			return fmt.Errorf("bhyve exited due a bhyve error (status 4) - %w - stderr: %s",
				exitedErr, o.stderr.String())
		default:
			return fmt.Errorf("bhyve exited due to an unknown bhyve error (status %d) - %w - stderr: %s",
				execExitErr.ExitCode(), exitedErr, o.stderr.String())
		}
	} else {
		log.Printf("bhyve process exited unexpectedly - %s - stderr: %s",
			exitedErr, o.stderr.String())
	}

	return nil
}

func newUnixSocket(ctx context.Context, filePath string, perm os.FileMode) (<-chan acceptResult, io.Closer, error) {
	_ = os.Remove(filePath)

	listener, err := net.Listen("unix", filePath)
	if err != nil {
		return nil, nil, err
	}

	err = os.Chmod(filePath, perm)
	if err != nil {
		_ = listener.Close()
		_ = os.Remove(filePath)

		return nil, nil, fmt.Errorf("failed to chmod unix socket - %w", err)
	}

	results := make(chan acceptResult)

	go func() {
		for {
			var r acceptResult
			r.conn, r.err = listener.Accept()

			select {
			case <-ctx.Done():
				if r.conn != nil {
					_ = r.conn.Close()
				}
				return
			case results <- r:
				if r.err != nil {
					return
				}
			}
		}
	}()

	return results, listener, nil
}

type acceptResult struct {
	conn net.Conn
	err  error
}

func powerStateRequestsHanlder(ctx context.Context, accepts <-chan acceptResult) <-chan powerStateRequest {
	requests := make(chan powerStateRequest)

	go func() {
		for {
			select {
			case <-ctx.Done():
				// TODO: Tell something about this.
				log.Printf("power state handler exiting - %s", ctx.Err())

				return
			case accept := <-accepts:
				if accept.err != nil {
					return
				}

				err := handlePowerStateRequest(ctx, requests, accept.conn)
				if err != nil {
					// TODO: Tell something about this.
					log.Printf("power state handler exiting - %s", err)

					return
				}
			}
		}
	}()

	return requests
}

func handlePowerStateRequest(ctx context.Context, requests chan powerStateRequest, conn net.Conn) error {
	defer conn.Close()

	sendMsgFn := func(msg string) error {
		_, err := conn.Write([]byte(msg + "\n"))
		return err
	}

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	scanner := bufio.NewScanner(io.LimitReader(conn, 512))

	if !scanner.Scan() {
		return nil
	}

	text := scanner.Text()

	state := powerStateFromString(text)
	if state == unknownPowerState {
		sendMsgFn(fmt.Sprintf("error: unknown power state: %q", text))

		return nil
	}

	cb := make(chan error, 1)

	select {
	case <-ctx.Done():
		sendMsgFn(fmt.Sprintf("error: %s", ctx.Err().Error()))

		return ctx.Err()
	case requests <- powerStateRequest{
		newState: state,
		cb:       cb,
	}:
	}

	select {
	case <-ctx.Done():
		sendMsgFn(fmt.Sprintf("error: %s", ctx.Err().Error()))

		return ctx.Err()
	case err := <-cb:
		if err != nil {
			sendMsgFn(fmt.Sprintf("error: %s", err.Error()))
		} else {
			sendMsgFn("")
		}
	}

	return nil
}

func powerStateFromString(str string) powerState {
	switch str {
	case onPowerState.String():
		return onPowerState
	case acpiOffPowerState.String():
		return acpiOffPowerState
	case acpiRebootPowerState.String():
		return acpiRebootPowerState
	case pullPowerCablePowerState.String():
		return pullPowerCablePowerState
	case pullPowerCableRebootPowerState.String():
		return pullPowerCableRebootPowerState
	default:
		return unknownPowerState
	}
}

type powerState int

func (o powerState) String() string {
	switch o {
	case onPowerState:
		return "on"
	case acpiOffPowerState:
		return "off"
	case acpiRebootPowerState:
		return "reboot"
	case pullPowerCablePowerState:
		return "pull-cable"
	case pullPowerCableRebootPowerState:
		return "pull-cable-reboot"
	default:
		return "unknown power state"
	}
}

const (
	unknownPowerState powerState = iota
	onPowerState
	acpiOffPowerState
	acpiRebootPowerState
	pullPowerCablePowerState
	pullPowerCableRebootPowerState
)

type powerStateRequest struct {
	newState powerState
	cb       chan error
}

type connManagerConfig struct {
	accepts  <-chan acceptResult
	logFile  io.Writer
	connDest io.Writer
}

func newConnManager(ctx context.Context, config connManagerConfig) *connManager {
	cm := &connManager{
		config:   config,
		fromProc: make(chan rwEvent),
		done:     make(chan struct{}),
		wait:     make(chan struct{}),
	}

	go cm.manageConnsLoop(ctx)

	return cm
}

type connManager struct {
	config   connManagerConfig
	fromProc chan rwEvent
	once     sync.Once
	done     chan struct{}
	wait     chan struct{}
	err      error
}

func (o *connManager) close() {
	o.once.Do(func() {
		close(o.done)
		<-o.wait
	})
}

func (o *connManager) Write(b []byte) (int, error) {
	cb := make(chan rwEventResult, 1)

	select {
	case <-o.wait:
		return 0, o.err
	case o.fromProc <- rwEvent{
		b:  b,
		cb: cb,
	}:
	}

	select {
	case <-o.wait:
		return 0, o.err
	case result := <-cb:
		return result.n, result.err
	}
}

func (o *connManager) writer() chan<- rwEvent {
	return o.fromProc
}

func (o *connManager) manageConnsLoop(ctx context.Context) {
	currentConns := make(map[net.Conn]struct{})
	defer func() {
		if o.err == nil {
			o.err = errors.New("unknown error")
		}

		errMsg := []byte(o.err.Error() + "\n")

		for conn := range currentConns {
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))

			_, _ = conn.Write(errMsg)

			_ = conn.Close()

			delete(currentConns, conn)
		}

		close(o.wait)
	}()

	closeConns := make(chan net.Conn)

	fromProxBufMaxBytes := 1024
	fromProcBuf := bytes.NewBuffer(nil)

	for {
		select {
		case <-ctx.Done():
			o.err = ctx.Err()
			return
		case <-o.done:
			o.err = errors.New("daemon has been shutdown")
			return
		case accept := <-o.config.accepts:
			if accept.err != nil {
				o.err = fmt.Errorf("ipc socket listener exited with error - %w",
					accept.err)

				log.Println(o.err.Error())

				return
			}

			setDeadLineErr := accept.conn.SetReadDeadline(time.Now().Add(time.Second))
			if setDeadLineErr == nil {
				options := make([]byte, 1)

				_, _ = accept.conn.Read(options)

				if fromProcBuf.Len() > 0 && options[0]&bufferedOutputClientFlag != 0 {
					_, err := fromProcBuf.WriteTo(accept.conn)
					if err != nil {
						_ = accept.conn.Close()
						continue
					}
				}

				_ = accept.conn.SetReadDeadline(time.Time{})
			}

			go func() {
				_, _ = io.Copy(o.config.connDest, accept.conn)
				select {
				case closeConns <- accept.conn:
				case <-ctx.Done():
					_ = accept.conn.Close()
				}
			}()

			currentConns[accept.conn] = struct{}{}
		case closeThis := <-closeConns:
			_ = closeThis.Close()
			delete(currentConns, closeThis)
		case write := <-o.fromProc:
			var n int
			var err error
			if o.config.logFile != nil {
				n, err = o.config.logFile.Write(write.b)
			} else {
				n = len(write.b)
			}

			write.cb <- rwEventResult{
				n:   n,
				err: err,
			}

			if len(currentConns) == 0 {
				fromProcBuf.Write(write.b)

				if fromProcBuf.Len() > fromProxBufMaxBytes {
					discard := fromProcBuf.Len() - fromProxBufMaxBytes

					io.CopyN(io.Discard, fromProcBuf, int64(discard))
				}
			}

			for conn := range currentConns {
				_, connErr := conn.Write(write.b)
				if connErr != nil {
					_ = conn.Close()
					delete(currentConns, conn)
				}
			}
		}
	}
}

// TODO: Remove.
type writerProxy struct {
	ctx    context.Context
	writes chan<- rwEvent
}

type rwEvent struct {
	b  []byte
	cb chan<- rwEventResult
}

type rwEventResult struct {
	n   int
	err error
}

func (o *writerProxy) Write(b []byte) (int, error) {
	cb := make(chan rwEventResult, 1)

	select {
	case o.writes <- rwEvent{
		b:  b,
		cb: cb,
	}:
	case <-o.ctx.Done():
		return 0, o.ctx.Err()
	}

	select {
	case result := <-cb:
		return result.n, result.err
	case <-o.ctx.Done():
		return 0, o.ctx.Err()
	}
}

func power(flagSet *flag.FlagSet) error {
	_ = flagSet.Parse(os.Args[2:])

	vmName := flagSet.Arg(0)
	if vmName == "" {
		return errors.New("please specify a vm name as the first non-flag argument")
	}

	powerStateStr := flagSet.Arg(1)
	if powerStateStr == "" {
		return errors.New("please specify a power state as the last non-flag argument")
	}

	state := powerStateFromString(powerStateStr)
	if state == unknownPowerState {
		return fmt.Errorf("unknown power state type: %q", powerStateStr)
	}

	conn, err := net.Dial("unix", powerStateSocketPath(vmName))
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
	waitForSocketToClose := flagSet.Bool(
		"w",
		false,
		"Do not exit if stdin is closed (useful for writing to stdin in a shell,\n"+
			"closing it, and waiting until the daemon shuts down)")

	noBufferedOutput := flagSet.Bool(
		"q",
		false,
		"Do not retrieve buffered output from daemon's child process")

	_ = flagSet.Parse(os.Args[2:])

	if flagSet.NArg() == 0 {
		return errors.New("please specify a vm name as the first non-flag argument")
	}

	conn, err := net.Dial("unix", consoleSocketPath(flagSet.Arg(0)))
	if err != nil {
		return fmt.Errorf("failed to open console unix socket - %w", err)
	}
	defer conn.Close()

	ctx, cancelFn := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancelFn()

	var clientFlags byte
	if !*noBufferedOutput {
		clientFlags |= bufferedOutputClientFlag
	}

	_, err = conn.Write([]byte{clientFlags})
	if err != nil {
		return fmt.Errorf("failed to write client flags to socket - %s", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, os.Stdin)
		if !*waitForSocketToClose {
			done <- err
		}
	}()

	go func() {
		_, err := io.Copy(os.Stdout, conn)
		done <- err
	}()

	select {
	case err = <-done:
		return err
	case <-ctx.Done():
		return nil
	}
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
