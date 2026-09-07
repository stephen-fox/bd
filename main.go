// bd
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gitlab.com/stephen-fox/bd/internal/bhyver"
	"gitlab.com/stephen-fox/bd/internal/config"
	"gitlab.com/stephen-fox/bd/internal/hsio"
	"gitlab.com/stephen-fox/bd/internal/lctx"
	"gitlab.com/stephen-fox/bd/internal/nettools"
	"gitlab.com/stephen-fox/bd/internal/osmaybe"
	"gitlab.com/stephen-fox/bd/internal/passfd"
	"gitlab.com/stephen-fox/bd/internal/rwsrv"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const (
	appName = "bd"
	version = "0.1.0"

	usage = appName + `

SYNOPSIS
  ` + appName + ` version
  ` + appName + ` [OPTIONS] install [INIT-OPTIONS]
  ` + appName + ` [OPTIONS] genmac [GENMAC-OPTIONS]
  ` + appName + ` [OPTIONS] new [NEW-OPTIONS] VM-NAME
  ` + appName + ` [OPTIONS] ls [LIST-OPTIONS] [VM-NAMES...]
  ` + appName + ` [OPTIONS] status [STATUS-OPTIONS] VM-NAME
  ` + appName + ` [OPTIONS] start [START-OPTIONS] VM-NAME
  ` + appName + ` [OPTIONS] restart [RESTART-OPTIONS] VM-NAME
  ` + appName + ` [OPTIONS] autostart [AUTOSTART-OPTIONS]
  ` + appName + ` [OPTIONS] stop [STOP-OPTIONS] VM-NAME
  ` + appName + ` [OPTIONS] pull-cable [PULLCABLE-OPTIONS] VM-NAME
  ` + appName + ` [OPTIONS] console [CONSOLE-OPTIONS] VM-NAME

DESCRIPTION

(TODO ...)

OPTIONS
`

	singleVmModeArg = "M"
	dryRunModeArg   = "D"

	singleVmModeDesc = "Automatically pick the vm (only permitted if one vm is running)"

	vmSocketsPerm = fs.FileMode(0o660)
)

// Various directory and configuration file paths.
const (
	appConfigDirPath         = "/usr/local/etc/" + appName
	appConfigFilePath        = appConfigDirPath + "/" + appName + ".conf"
	topLevelVmConfigsDirPath = appConfigDirPath + "/vm-configs"

	appRuntimeDirPath          = "/usr/local/var/" + appName
	topLevelVmVariablesDirPath = appRuntimeDirPath + "/vm-variables"
	topLevelVmRuntimeDirPath   = appRuntimeDirPath + "/vm-runtime"
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
	case "version":
		fmt.Println(version)
		return nil
	case "install":
		return doInstall(flagSet)
	case "genmac":
		return genmac()
	case "daemon":
		return daemon(flagSet)
	case "console-daemon":
		return consoleDaemon(flagSet)
	case "new":
		return newvm(flagSet)
	case "list", "ls":
		return list(flagSet)
	case "status":
		return status(flagSet)
	case "start":
		return start(flagSet)
	case "autostart":
		return autostart(flagSet)
	case "restart":
		return restart(flagSet)
	case "stop":
		return stop(flagSet)
	case "pull-cable":
		return pullCable(flagSet)
	case "console":
		return console(flagSet)
	default:
		return fmt.Errorf("unknown command: '%s'", flag.Arg(0))
	}
}

func genmac() error {
	mac, err := nettools.GenMacAddr()
	if err != nil {
		return err
	}

	os.Stdout.WriteString(mac + "\n")

	return nil
}

func doInstall(flagSet *flag.FlagSet) error {
	_ = flagSet.Parse(os.Args[2:])

	err := checkIfRunningAsRoot()
	if err != nil {
		return err
	}

	err = os.MkdirAll(appConfigDirPath, 0o755)
	if err != nil {
		return fmt.Errorf("failed to create application config directory - %w", err)
	}

	_, err = os.Stat(appConfigFilePath)
	if err == nil {
		return fmt.Errorf("an application-level config file already exists at: %q",
			appConfigFilePath)
	}

	var buf bytes.Buffer

	err = config.NewAppConfigFile(&buf)
	if err != nil {
		return fmt.Errorf("failed to generate application-level config - %w", err)
	}

	err = os.WriteFile(appConfigFilePath, buf.Bytes(), 0o600)
	if err != nil {
		return fmt.Errorf("failed to create application-level config-file - %w", err)
	}

	err = os.MkdirAll(topLevelVmVariablesDirPath, 0o700)
	if err != nil {
		return fmt.Errorf("failed to create variables directory - %w", err)
	}

	os.Stderr.WriteString("created configuration file at: ")
	fmt.Println(appConfigFilePath)
	os.Stderr.WriteString("make sure to update its default values\n")

	return nil
}

func daemon(flagSet *flag.FlagSet) error {
	foreground := flagSet.Bool(
		"F",
		false,
		"Stay in the foreground rather than exec'ing into background")

	_ = flagSet.Parse(os.Args[2:])

	vmName, err := getOnlyOneNonFlagArg(flagSet, "the vm name")
	if err != nil {
		return err
	}

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

	const childEnvName = appName + "_" + "child"

	isChild := os.Getenv(childEnvName) != "" || *foreground
	if !isChild {
		err := checkIfRunningAsRoot()
		if err != nil {
			return err
		}

		exePath, err := os.Executable()
		if err != nil {
			return fmt.Errorf("failed to get path of process' executable - %w", err)
		}

		restarted := exec.Command(exePath, os.Args[1:]...)

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

		err = restarted.Start()
		if err != nil {
			return fmt.Errorf("failed to exec to background - %w", err)
		}

		pidFilePath := "/var/run/" + appName + "-" + vmName + ".pid"

		_ = os.Remove(pidFilePath)

		err = os.WriteFile(
			pidFilePath,
			[]byte(fmt.Sprintf("%d\n", restarted.Process.Pid)),
			0o644)
		if err != nil {
			_ = restarted.Process.Kill()

			return fmt.Errorf("failed to write pid file '%s' - %w",
				pidFilePath, err)
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

	if *foreground {
		log.SetFlags(log.LstdFlags)
	} else {
		syslogWriter, err := syslog.New(syslog.LOG_DAEMON|syslog.LOG_INFO, appName+" - "+vmName)
		if err != nil {
			return fmt.Errorf("failed to open syslog - %w", err)
		}

		log.SetOutput(syslogWriter)
	}

	appConfig, err := loadAppConfig()
	if err != nil {
		return fmt.Errorf("failed to load application config - %w", err)
	}

	vmVariablesDir := filepath.Join(topLevelVmVariablesDirPath, vmName)

	err = os.MkdirAll(vmVariablesDir, 0o755)
	if err != nil {
		return fmt.Errorf("failed to create vm variables dir - %w", err)
	}

	err = os.Chmod(vmVariablesDir, 0o700)
	if err != nil {
		return fmt.Errorf("failed to chmod vm variables dir - %w", err)
	}

	vmVars, err := config.LoadVariables(config.LoadVariablesArgs{
		VmName:             vmName,
		VmVariablesDirPath: vmVariablesDir,
		AppConfig:          appConfig,
		SkipLoading:        []config.VariableType{config.PrestartVariableType},
	})
	if err != nil {
		return fmt.Errorf("failed to load vm variables - %w", err)
	}

	vmConfig, err := loadVmConfig(vmName)
	if err != nil {
		return fmt.Errorf("failed to load vm config for %q - %w",
			vmName, err)
	}

	vmRuntimeDir := vmRuntimeDirPath(vmName)

	err = os.MkdirAll(vmRuntimeDir, 0o755)
	if err != nil {
		return fmt.Errorf("failed to create vm dir path - %w",
			err)
	}

	err = os.Chmod(vmRuntimeDir, 0o700)
	if err != nil {
		return fmt.Errorf("failed to chmod vm runtime dir path %q - %w",
			vmRuntimeDir, err)
	}

	ctx, cancelFn := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	defer cancelFn()

	err = vmConfig.CreatePrestarts(ctx, vmVars)
	if err != nil {
		return fmt.Errorf("failed to create prestart dependencies - %w", err)
	}
	defer func() {
		cleanupCtx, cancelFn := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelFn()

		vmConfig.CleanupPrestarts(cleanupCtx, vmVars, log.Default())
	}()

	err = vmVars.SaveVariableToFs(ctx)
	if err != nil {
		return fmt.Errorf("failed to write vm variables to disk - %w", err)
	}

	// Note: Rendering the bhyve arguments must come last because
	// other functionality like prestart deps may add new variables.
	bhyveArgs, err := vmConfig.RenderBhyveArgs(vmVars, appConfig)
	if err != nil {
		return fmt.Errorf("failed to render bhyve args - %w", err)
	}

	statusListener, err := lctx.ListenUnixPath(
		ctx,
		statusSocketPath(vmRuntimeDir),
		vmSocketsPerm)
	if err != nil {
		return fmt.Errorf("failed to create status unix listener socket - %w", err)
	}
	defer statusListener.Close()

	statusRequests := bhyver.StatusRequestsHandler(ctx, statusListener)

	powerStateListener, err := lctx.ListenUnixPath(
		ctx,
		powerStateSocketPath(vmRuntimeDir),
		vmSocketsPerm)
	if err != nil {
		return fmt.Errorf("failed to create power state listener unix socket - %w", err)
	}
	defer powerStateListener.Close()

	powerStateRequests := bhyver.PowerStateRequestsHandler(ctx, powerStateListener)

	var optConsoleDaemon *consoleDaemonChild
	var optConsoleDaemonDone <-chan struct{}
	var optConsoleDaemonConn *net.UnixConn

	if vmConfig.General.SerialConsole {
		log.Println("setting up console daemon...")

		optConsoleDaemon, err = execConsoleDaemon(ctx, vmRuntimeDir)
		if err != nil {
			return fmt.Errorf("failed to start console daemon - %w", err)
		}
		defer optConsoleDaemon.Kill()

		optConsoleDaemonDone = optConsoleDaemon.Done()
		optConsoleDaemonConn = optConsoleDaemon.FdConn()

		log.Println("console daemon started successfully")
	}

	// TOOD: Send bhyve stderr to syslog.
	runner := bhyver.StartRunner(ctx, bhyver.RunnerConfig{
		VmName:           vmName,
		BhyveArgs:        bhyveArgs,
		StatusRequests:   statusRequests,
		PowerRequests:    powerStateRequests,
		OptConsoleFdConn: optConsoleDaemonConn,
	})

	// Wait to see if runner exits due to a bhyve error.
	select {
	case <-runner.Done():
		// TODO: In jail scenarios, it appears that syslogd
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

func newvm(flagSet *flag.FlagSet) error {
	sizeGb := flagSet.Uint64("s", 100, "VM disk size in gb")

	dryRun := flagSet.Bool(dryRunModeArg, false, "Run in dry-run mode; logs operations but does not actually create a vm")

	_ = flagSet.Parse(os.Args[2:])

	if !*dryRun {
		err := checkIfRunningAsRoot()
		if err != nil {
			return err
		}
	}

	vmName, err := getOnlyOneNonFlagArg(flagSet, "the vm name of the vm create")
	if err != nil {
		return err
	}

	appConfig, err := loadAppConfig()
	if err != nil {
		return err
	}

	maybeDo := osmaybe.MaybeDo{
		DryRunMode: *dryRun,
	}

	if maybeDo.DryRunMode {
		maybeDo.Logger = log.New(log.Default().Writer(), "[dry-run] ", log.Default().Flags())
	}

	var undo undoFns

	vmVariablesDir := filepath.Join(topLevelVmVariablesDirPath, vmName)

	err = maybeDo.Mkdir(vmVariablesDir, 0o700)
	if err != nil {
		return fmt.Errorf("failed to create vm variables dir: %q - %w",
			vmVariablesDir, err)
	}

	undo = append(undo, func() { maybeDo.Remove(vmVariablesDir) })

	vmStorageDir := vmStorageDirPath(vmName, appConfig)

	vmStorageZfsId := strings.TrimLeft(vmStorageDir, "/")

	zfsCreate := exec.Command("/sbin/zfs", "create", vmStorageZfsId)

	output, err := maybeDo.ExecCombinedOutput(zfsCreate)
	if err != nil {
		undo.undoFromEnd()

		return fmt.Errorf("failed to create zfs dataset for vm storage: %q - %w (output: %q)",
			vmStorageZfsId, err, output)
	}

	undo = append(undo, func() { maybeDo.ExecCombinedOutput(exec.Command("/sbin/zfs", "destroy", vmStorageZfsId)) })

	undo = append(undo, func() { maybeDo.Remove(vmStorageDir) })

	err = maybeDo.Chmod(vmStorageDir, 0o700)
	if err != nil {
		undo.undoFromEnd()

		return fmt.Errorf("failed to chmod vm storage dir: %q - %w",
			vmStorageDir, err)
	}

	vmConfigPath := vmConfigFilePath(vmName)
	vmConfigDir := filepath.Dir(vmConfigPath)

	err = maybeDo.MkdirAll(vmConfigDir, 0o755)
	if err != nil {
		undo.undoFromEnd()

		return fmt.Errorf("failed to create vm config dir: %q - %w",
			vmConfigDir, err)
	}

	undo = append(undo, func() { maybeDo.Remove(vmConfigDir) })

	err = maybeDo.Chmod(vmConfigDir, 0o700)
	if err != nil {
		undo.undoFromEnd()

		return fmt.Errorf("failed to chmod vm config dir: %q - %w",
			vmConfigDir, err)
	}

	var buf bytes.Buffer

	err = config.NewVmConfigFile(&buf)
	if err != nil {
		undo.undoFromEnd()

		return fmt.Errorf("failed to generate vm config file - %w",
			err)
	}

	err = maybeDo.WriteFileHumanReadable(vmConfigPath, buf.Bytes(), 0o644)
	if err != nil {
		undo.undoFromEnd()

		return fmt.Errorf("failed to write vm config file: %q - %w",
			vmConfigPath, err)
	}

	undo = append(undo, func() { maybeDo.Remove(vmConfigPath) })

	err = maybeDo.Chmod(vmConfigDir, 0o700)
	if err != nil {
		undo.undoFromEnd()

		return fmt.Errorf("failed to chmod vm storage dir: %q - %w",
			vmStorageDir, err)
	}

	vmDiskPath := filepath.Join(vmStorageDir, "disk0.img")

	truncate := exec.Command("/usr/bin/truncate",
		"-s", fmt.Sprintf("%dG", *sizeGb),
		vmDiskPath)

	truncateOutput, err := maybeDo.ExecCombinedOutput(truncate)
	if err != nil {
		undo.undoFromEnd()

		return fmt.Errorf("failed to create vm disk: %q - %w - (truncate output: %s)",
			vmDiskPath, err, truncateOutput)
	}

	undo = append(undo, func() { maybeDo.Remove(vmDiskPath) })

	origUefiVarsFileInfo, err := os.Stat("/usr/local/share/uefi-firmware/BHYVE_UEFI_VARS.fd")
	if err == nil && !origUefiVarsFileInfo.IsDir() {
		uefiVarsFilePath := filepath.Join(vmStorageDir, config.UefiVarsFileName)

		output, err := maybeDo.ExecCombinedOutput(exec.Command(
			"/bin/cp",
			"/usr/local/share/uefi-firmware/BHYVE_UEFI_VARS.fd",
			uefiVarsFilePath))
		if err != nil {
			return fmt.Errorf("failed to copy uefi variables file to vm storage dir - %w - output: %q",
				err, output)
		}

		undo = append(undo, func() { maybeDo.Remove(uefiVarsFilePath) })
	}

	os.Stderr.WriteString("vm config file can be found at: ")
	os.Stdout.WriteString(vmConfigPath + "\n")

	return nil
}

type undoFns []func()

func (o undoFns) undoFromEnd() {
	if len(o) == 0 {
		return
	}

	for i := len(o) - 1; i > -1; i-- {
		o[i]()
	}
}

func list(flagSet *flag.FlagSet) error {
	// doNotFailIfDaemonIsStopped := flagSet.Bool(
	// 	"",
	// 	false,
	// 	"")

	_ = flagSet.Parse(os.Args[2:])

	// TODO: Maybe some day allow this for non-root users.
	err := checkIfRunningAsRoot()
	if err != nil {
		return err
	}

	var status statusGetter

	if flagSet.NArg() == 0 {
		err := forEachVm(context.Background(), func(_ context.Context, vmName string) error {
			status.get(vmName)

			return nil
		})
		if err != nil {
			return err
		}
	} else {
		for _, vmName := range flag.Args() {
			status.get(vmName)
		}
	}

	if status.buf.Len() == 0 {
		return nil
	}

	fmt.Print(status.buf.String())

	if !status.oneSuccess {
		os.Exit(1)
	}

	return nil
}

func forEachVm(ctx context.Context, fn func(ctx context.Context, vmName string) error) error {
	entries, err := os.ReadDir(topLevelVmConfigsDirPath)
	if err != nil {
		return fmt.Errorf("failed to read virtual machines configuration dir - %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Keep going.
		}

		vmName := entry.Name()

		err := fn(ctx, vmName)
		if err != nil {
			return err
		}
	}

	return nil
}

type statusGetter struct {
	buf        bytes.Buffer
	verbose    bool
	oneSuccess bool
}

func (o *statusGetter) get(vmName string) {
	o.buf.WriteString(vmName)
	o.buf.WriteByte('\t')

	status, _, err := vmStatus(vmName, o.verbose)
	if err != nil {
		o.buf.WriteString("unknown (")
		o.buf.WriteString(err.Error())
		o.buf.WriteString(")")
	} else {
		o.buf.WriteString(status)
		o.oneSuccess = true
	}

	o.buf.WriteByte('\n')
}

func status(flagSet *flag.FlagSet) error {
	// doNotFailIfDaemonIsStopped := flagSet.Bool(
	// 	"",
	// 	false,
	// 	"")

	singleVmMode := flagSet.Bool(
		singleVmModeArg,
		false,
		singleVmModeDesc)

	dumpConfigMode := flagSet.Bool(
		"I",
		false,
		"Dump the VM's configuration to stdout as JSON")

	verbose := flagSet.Bool(
		"v",
		false,
		"Display additional information about the status of the VM")

	_ = flagSet.Parse(os.Args[2:])

	// TODO: Maybe some day allow this for non-root users.
	err := checkIfRunningAsRoot()
	if err != nil {
		return err
	}

	vmName, err := getVmFromFlagsOrSingleVmMode(flagSet, singleVmMode)
	if err != nil {
		return err
	}

	if *dumpConfigMode {
		vmConfig, err := loadVmConfig(vmName)
		if err != nil {
			return err
		}

		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")

		return encoder.Encode(vmConfig)
	}

	status := statusGetter{
		verbose: *verbose,
	}

	status.get(vmName)

	fmt.Print(status.buf.String())

	if status.oneSuccess {
		return nil
	} else {
		os.Exit(1)
	}

	return nil
}

func vmStatus(vmName string, verbose bool) (string, bool, error) {
	configFilePath := vmConfigFilePath(vmName)

	_, statErr := os.Stat(configFilePath)
	switch {
	case statErr == nil:
		// Keep going.
	case errors.Is(statErr, os.ErrNotExist):
		return "", false, fmt.Errorf("no such vm")
	default:
		return "", false, statErr
	}

	vmStatusSocketPath := statusSocketPath(vmRuntimeDirPath(vmName))

	conn, err := net.Dial("unix", vmStatusSocketPath)
	if err != nil {
		if verbose {
			return fmt.Sprintf("stopped (%s)", err), false, nil
		}

		return "stopped", false, nil
	}
	defer conn.Close()

	scanner := bufio.NewScanner(conn)

	if !scanner.Scan() {
		return "", false, fmt.Errorf("failed to read from socket - %w", scanner.Err())
	}

	return scanner.Text(), true, nil
}

func start(flagSet *flag.FlagSet) error {
	singleVmMode := flagSet.Bool(
		singleVmModeArg,
		false,
		singleVmModeDesc)

	_ = flagSet.Parse(os.Args[2:])

	// TODO: Maybe some day allow this for non-root users.
	err := checkIfRunningAsRoot()
	if err != nil {
		return err
	}

	vmName, err := getVmFromFlagsOrSingleVmMode(flagSet, singleVmMode)
	if err != nil {
		return err
	}

	_, gotResponse, _ := vmStatus(vmName, false)
	if gotResponse {
		return fmt.Errorf("vm is already running")
	}

	return execDaemon([]string{vmName})
}

func autostart(flagSet *flag.FlagSet) error {
	_ = flagSet.Parse(os.Args[2:])

	err := checkIfRunningAsRoot()
	if err != nil {
		return err
	}

	var autostartableVms []string
	var failedVms []string

	err = forEachVm(context.Background(), func(_ context.Context, vmName string) error {
		vmConfig, err := loadVmConfig(vmName)
		if err != nil {
			failedVms = append(failedVms, vmName)
			log.Printf("autostart: %q - failed to load config - %s", vmName, err)

			return nil
		}

		if !vmConfig.General.Autostart {
			return nil
		}

		_, gotResponse, _ := vmStatus(vmName, false)
		if gotResponse {
			log.Printf("autostart: %q - skipping because vm is already running", vmName)

			return nil
		}

		autostartableVms = append(autostartableVms, vmName)

		log.Printf("autostart: %q - starting daemon...", vmName)

		err = execDaemon([]string{vmName})
		if err != nil {
			failedVms = append(failedVms, vmName)
			log.Printf("autostart: %q - failed to exec daemon - %s", vmName, err)

			return nil
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("autostart failed - %w", err)
	}

	if len(failedVms) > 0 {
		return fmt.Errorf("failed to autostart %d/%d auto-startable virtual machine(s): %s",
			len(failedVms), len(autostartableVms), `"`+strings.Join(failedVms, `" `)+`"`)
	}

	if len(autostartableVms) == 0 {
		log.Println("no virtual machines were autostarted")
	} else {
		log.Printf("autostarted %d virtual machine(s): %s",
			len(autostartableVms), `"`+strings.Join(autostartableVms, `" `)+`"`)
	}

	return nil
}

func stop(flagSet *flag.FlagSet) error {
	singleVmMode := flagSet.Bool(
		singleVmModeArg,
		false,
		singleVmModeDesc)

	_ = flagSet.Parse(os.Args[2:])

	// TODO: Maybe some day allow this for non-root users.
	err := checkIfRunningAsRoot()
	if err != nil {
		return err
	}

	vmName, err := getVmFromFlagsOrSingleVmMode(flagSet, singleVmMode)
	if err != nil {
		return err
	}

	vmRuntimeDir := vmRuntimeDirPath(vmName)

	conn, err := net.Dial("unix", powerStateSocketPath(vmRuntimeDir))
	if err != nil {
		return fmt.Errorf("failed to open power state unix socket (is the vm currently running?) - %w", err)
	}
	defer conn.Close()

	_, err = conn.Write([]byte(bhyver.AcpiOffPowerState.String() + "\n"))
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

func restart(flagSet *flag.FlagSet) error {
	singleVmMode := flagSet.Bool(
		singleVmModeArg,
		false,
		singleVmModeDesc)

	_ = flagSet.Parse(os.Args[2:])

	// TODO: Maybe some day allow this for non-root users.
	err := checkIfRunningAsRoot()
	if err != nil {
		return err
	}

	vmName, err := getVmFromFlagsOrSingleVmMode(flagSet, singleVmMode)
	if err != nil {
		return err
	}

	vmRuntimeDir := vmRuntimeDirPath(vmName)

	_, gotResponse, _ := vmStatus(vmName, false)

	if gotResponse {
		conn, err := net.Dial("unix", powerStateSocketPath(vmRuntimeDir))
		if err != nil {
			return fmt.Errorf("failed to open power state unix socket (is the vm currently running?) - %w", err)
		}
		defer conn.Close()

		_, err = conn.Write([]byte(bhyver.AcpiOffPowerState.String() + "\n"))
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

		conn.Close()
	}

	return execDaemon([]string{vmName})
}

func execDaemon(args []string) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get current process' executable path - %w", err)
	}

	argsAndCommand := make([]string, len(args)+1)
	argsAndCommand[0] = "daemon"
	copy(argsAndCommand[1:], args)

	daemon := exec.Command(exePath, argsAndCommand...)
	daemon.Stderr = os.Stderr
	daemon.Stdout = os.Stdout

	return daemon.Run()
}

func pullCable(flagSet *flag.FlagSet) error {
	// doNotFailIfDaemonIsStopped := flagSet.Bool(
	// 	"",
	// 	false,
	// 	"")

	singleVmMode := flagSet.Bool(
		singleVmModeArg,
		false,
		singleVmModeDesc)

	_ = flagSet.Parse(os.Args[2:])

	// TODO: Maybe some day allow this for non-root users.
	err := checkIfRunningAsRoot()
	if err != nil {
		return err
	}

	vmName, err := getVmFromFlagsOrSingleVmMode(flagSet, singleVmMode)
	if err != nil {
		return err
	}

	vmRuntimeDir := vmRuntimeDirPath(vmName)

	conn, err := net.Dial("unix", powerStateSocketPath(vmRuntimeDir))
	if err != nil {
		return fmt.Errorf("failed to open power state unix socket - %w", err)
	}
	defer conn.Close()

	_, err = conn.Write([]byte(bhyver.PullPowerCablePowerState.String() + "\n"))
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
		singleVmModeArg,
		false,
		singleVmModeDesc)

	allowStdinToClose := flagSet.Bool(
		"w",
		false,
		"Do not exit if stdin is closed (useful for writing to stdin in a shell,\n"+
			"closing it, and waiting until the daemon shuts down)")

	_ = flagSet.Parse(os.Args[2:])

	// TODO: Maybe some day allow this for non-root users.
	err := checkIfRunningAsRoot()
	if err != nil {
		return err
	}

	vmName, err := getVmFromFlagsOrSingleVmMode(flagSet, singleVmMode)
	if err != nil {
		return err
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

func statusSocketPath(runtimeDirPath string) string {
	return filepath.Join(runtimeDirPath, "status.sock")
}

func powerStateSocketPath(runtimeDirPath string) string {
	return filepath.Join(runtimeDirPath, "power.sock")
}

func vmRuntimeDirPath(vmName string) string {
	return filepath.Join(topLevelVmRuntimeDirPath, vmName)
}

func loadAppConfig() (*config.AppConfig, error) {
	_, err := os.Stat(appConfigDirPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat app config dir - make sure to run '%s init' first (%w)",
			appName, err)
	}

	f, err := os.Open(appConfigFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open app config file - %w", err)
	}
	defer f.Close()

	appConfig, err := config.ParseAppConfig(f)
	if err != nil {
		return nil, fmt.Errorf("failed to parse app config - %w", err)
	}

	return appConfig, nil
}

func loadVmConfig(vmName string) (*config.VmConfig, error) {
	configDir := vmConfigDirPath(vmName)

	_, err := os.Stat(configDir)
	if err != nil {
		return nil, fmt.Errorf("failed to stat vm config dir (does the vm exist?) - %w", err)
	}

	configPath := vmConfigFilePath(vmName)

	f, err := os.Open(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open vm config file - %w", err)
	}
	defer f.Close()

	vmConfig, err := config.ParseVmConfig(f, vmName)
	if err != nil {
		return nil, fmt.Errorf("failed to parse vm config - %w", err)
	}

	return vmConfig, nil
}

func vmConfigFilePath(vmName string) string {
	return filepath.Join(vmConfigDirPath(vmName), "vm.conf")
}

func vmConfigDirPath(vmName string) string {
	return filepath.Join(topLevelVmConfigsDirPath, vmName)
}

func vmStorageDirPath(vmName string, appConfig *config.AppConfig) string {
	return filepath.Join(appConfig.General.VmsStorageDir, vmName)
}

func getOnlyOneNonFlagArg(flagSet *flag.FlagSet, lookingFor string) (string, error) {
	switch flagSet.NArg() {
	case 0:
		return "", fmt.Errorf("please specify only one non-flag argument: %s", lookingFor)
	case 1:
		return flagSet.Arg(0), nil
	default:
		return "", fmt.Errorf("please specify only one non-flag argument (got %d arguments)",
			flagSet.NArg())
	}
}

func getVmFromFlagsOrSingleVmMode(flagSet *flag.FlagSet, singleVmMode *bool) (string, error) {
	switch flagSet.NArg() {
	case 0:
		if !*singleVmMode {
			return "", errors.New("please specify a vm name as the first non-flag argument")
		}

		entries, err := os.ReadDir(topLevelVmRuntimeDirPath)
		if err != nil {
			return "", fmt.Errorf("failed to read vm runtime dir - %w", err)
		}

		if len(entries) != 1 {
			return "", fmt.Errorf("expected only one vm - found: %d",
				len(entries))
		}

		return entries[0].Name(), nil
	case 1:
		return flagSet.Arg(0), nil
	default:
		return "", fmt.Errorf("please specify only one non-flag argument (got %d arguments)",
			flagSet.NArg())
	}
}

func checkIfRunningAsRoot() error {
	uid := os.Getuid()
	gid := os.Getgid()

	switch {
	case uid != 0:
		return fmt.Errorf("this mode requires the program be run as root (current uid is: %d)", uid)
	case gid != 0:
		return fmt.Errorf("this mode requires the program be run as root (current gid is: %d)", gid)
	default:
		return nil
	}
}
