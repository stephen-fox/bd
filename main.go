// bhyved
package main

import (
	"bufio"
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
	"syscall"

	"gitlab.com/stephen-fox/bhyved/internal/bhyver"
	"gitlab.com/stephen-fox/bhyved/internal/fdserver"
	"gitlab.com/stephen-fox/bhyved/internal/hsio"
	"gitlab.com/stephen-fox/bhyved/internal/lctx"
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
			return errors.New("TODO: this needs to be re-implemented :(")
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

	powerStateListener, err := lctx.ListenUnixPath(
		ctx,
		powerStateSocketPath(vmName),
		socketMode.mode)
	if err != nil {
		return fmt.Errorf("failed to create power state unix socket - %w", err)
	}
	defer powerStateListener.Close()

	powerStateRequests := bhyver.PowerStateRequestsHanlder(ctx, powerStateListener)

	consoleListener, err := lctx.ListenUnixPath(
		ctx,
		consoleSocketPath(vmName),
		socketMode.mode)
	if err != nil {
		return fmt.Errorf("failed to create console unix socket - %w", err)
	}
	defer consoleListener.Close()

	consoleFds := fdserver.ServeListener(ctx, consoleListener)

	// TODO: Fix serial console log file.
	runner := bhyver.NewRunner(vmName, flagSet.Args(), powerStateRequests, consoleFds)

	return runner.Loop(ctx)
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

	state := bhyver.PowerStateFromString(powerStateStr)
	if state == bhyver.UnknownPowerState {
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
	_ = flagSet.Parse(os.Args[2:])

	if flagSet.NArg() == 0 {
		return errors.New("please specify a vm name as the first non-flag argument")
	}

	conn, err := net.Dial("unix", consoleSocketPath(flagSet.Arg(0)))
	if err != nil {
		return fmt.Errorf("failed to open console unix socket - %w", err)
	}
	defer conn.Close()

	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("expected *net.UnixConn - got %T", conn)
	}

	consoleStdin := hsio.NewWriteCloser()
	consoleStdout := hsio.NewReadCloser()

	consoleFdFns := hsio.NewFdUpdaterFnBuilder().
		AddWriter(consoleStdin).
		AddReader(consoleStdout).
		Build()

	hsio.NewUnixConnFdUpdater(context.Background(), unixConn, consoleFdFns)

	// ignoredSignals := make(chan os.Signal)
	// signal.Notify(ignoredSignals, syscall.SIGINT)
	// defer signal.Stop(ignoredSignals)

	// go func() {
	// 	for range ignoredSignals {
	// 	}
	// }()

	// stdinState, err := term.GetState(int(os.Stdin.Fd()))
	// if err != nil {
	// 	return err
	// }

	// stdoutState, err := term.GetState(int(os.Stdout.Fd()))
	// if err != nil {
	// 	return err
	// }

	errs := make(chan error, 2)

	go func() {
		_, err := io.Copy(os.Stdout, consoleStdout)
		errs <- err
	}()

	go func() {
		_, err := io.Copy(consoleStdin, os.Stdin)
		errs <- err
	}()

	return <-errs
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
