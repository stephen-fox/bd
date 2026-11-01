package app

import (
	"bytes"
	"context"
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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"codeberg.org/stephen-fox/bd/internal/bhyver"
	"codeberg.org/stephen-fox/bd/internal/config"
	"codeberg.org/stephen-fox/bd/internal/hsio"
	"codeberg.org/stephen-fox/bd/internal/lctx"
	"codeberg.org/stephen-fox/bd/internal/passfd"
	"codeberg.org/stephen-fox/bd/internal/rwsrv"
)

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

	vmVariablesDir := vmVariablesDirPath(vmName)

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

	vmConfig, err := loadVmConfig(vmName, appConfig)
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

	// We use context.Background for the listener because we
	// want it to service connectins until the moment bd exits.
	statusListener, err := lctx.ListenUnixPath(
		context.Background(),
		statusSocketPath(vmRuntimeDir),
		vmSocketsPerm)
	if err != nil {
		return fmt.Errorf("failed to create status unix listener socket - %w", err)
	}
	defer statusListener.Close()

	// We use context.Background for the listener because we
	// want it to service connectins until the moment bd exits.
	powerStateListener, err := lctx.ListenUnixPath(
		context.Background(),
		powerStateSocketPath(vmRuntimeDir),
		vmSocketsPerm)
	if err != nil {
		return fmt.Errorf("failed to create power state listener unix socket - %w", err)
	}
	defer powerStateListener.Close()

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
	runner, err := bhyver.ExecStartRunner(ctx, bhyver.RunnerConfig{
		VmName:              vmName,
		BhyveArgs:           bhyveArgs,
		PowerClientListener: powerStateListener,
		StatusClientLisener: statusListener,
		OptConsoleFdConn:    optConsoleDaemonConn,
	})
	if err != nil {
		return fmt.Errorf("failed to start bhyve runner - %w", err)
	}

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
	exePath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("failed to get current process' executable file path - %w", err)
	}

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

	consoleDaemon := exec.Command(exePath, "console-daemon")

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
