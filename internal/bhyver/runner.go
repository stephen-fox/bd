package bhyver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"codeberg.org/stephen-fox/bd/internal/lctx"
	"codeberg.org/stephen-fox/bd/internal/passfd"
)

// Various bhyve process status strings.
const (
	bhyveStatusExited  = "stopped"
	bhyveStatusRunning = "running"
)

const (
	runnerErrorPrefix = "error: "
	runnerStoppedMsg  = runnerErrorPrefix + "runner has stopped"
)

type RunnerConfig struct {
	VmName              string
	BhyveArgs           []string
	PowerClientListener *lctx.ListenerCtx
	StatusClientLisener *lctx.ListenerCtx

	OptConsoleFdConn *net.UnixConn
}

// ExecStartRunner instantiates a Runner and starts bhyve.
func ExecStartRunner(ctx context.Context, config RunnerConfig) (*Runner, error) {
	runner := &Runner{
		config:          config,
		powerRequests:   make(chan *PowerStateRequest),
		statusRequests:  make(chan *StatusRequest),
		shutdownStarted: make(chan error, 1),
		bhyveExited:     make(chan error, 1),
		stderr:          bytes.NewBuffer(nil),
		done:            make(chan struct{}),
	}

	err := runner.startBhyve(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to exec start bhyve - %w",
			err)
	}

	go config.PowerClientListener.Serve(runner.handlePowerClient)

	go config.StatusClientLisener.Serve(runner.handleStatusClient)

	go runner.loop(ctx)

	return runner, nil
}

// Runner operates a single bhyve process.
type Runner struct {
	config          RunnerConfig
	status          string
	powerRequests   chan *PowerStateRequest
	statusRequests  chan *StatusRequest
	shutdownOnce    sync.Once
	shutdownStarted chan error
	bhyveExited     chan error
	bhyveProc       *exec.Cmd
	stderr          *bytes.Buffer
	done            chan struct{}
	err             error
}

func (o *Runner) Done() <-chan struct{} {
	return o.done
}

func (o *Runner) Err() error {
	return o.err
}

func (o *Runner) startBhyve(ctx context.Context) error {
	if o.isBhyveRunning() {
		log.Printf("[warn] - bhyve start was attempted, but process is already running")

		return nil
	}

	log.Println("starting bhyve...")

	if o.vmmDeviceExists() {
		// TODO: We need to lookup the process first and
		// try sigkill'ing it.
		err := o.bhyvectlDestroy(ctx)
		if err != nil {
			return fmt.Errorf("failed to destroy existing vm device on startup - %w", err)
		}
	}

	o.stderr.Reset()

	// We purposely do not use exec.CommandContext in order
	// to avoid accidentally sending a signal to bhyve if
	// the upstream context is cancelled.
	bhyve := exec.Command("/usr/sbin/bhyve", o.config.BhyveArgs...)

	bhyve.SysProcAttr = &syscall.SysProcAttr{
		// We set Setpgid to true because, by default,
		// a signal sent to us will be automatically
		// sent to any children (i.e., pressing ctrl+c
		// to send SIGINT to bd will also send
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

	if o.config.OptConsoleFdConn != nil {
		stdin, err := bhyve.StdinPipe()
		if err != nil {
			return fmt.Errorf("failed to create stdin pipe - %w", err)
		}

		stdinFile, ok := stdin.(*os.File)
		if !ok {
			return fmt.Errorf("expected stdin pipe to be *os.File - got %T", stdin)
		}

		stdout, err := bhyve.StdoutPipe()
		if err != nil {
			return fmt.Errorf("failed to create stdout pipe - %w", err)
		}

		stdoutFile, ok := stdout.(*os.File)
		if !ok {
			return fmt.Errorf("expected stdout pipe to be *os.File - got %T", stdout)
		}

		err = passfd.Put(o.config.OptConsoleFdConn, stdinFile, stdoutFile)
		if err != nil {
			return fmt.Errorf("failed to send console fds to console daemon - %w", err)
		}
	}

	err := bhyve.Start()
	if err != nil {
		return fmt.Errorf("failed to start bhyve - %w", err)
	}

	o.bhyveProc = bhyve

	go func() {
		err := bhyve.Wait()

		log.Printf("bhyve exited - exec.cmd error is %v", err)

		o.bhyveExited <- err
	}()

	return nil
}

func (o *Runner) startShutdown(reason error) {
	o.shutdownOnce.Do(func() {
		o.shutdownStarted <- reason
	})
}

func (o *Runner) loop(ctx context.Context) {
	defer close(o.done)

	// Ensure any child go routines are killed if
	// an error occurs.
	var cancelFn func()
	ctx, cancelFn = context.WithCancel(ctx)
	defer cancelFn()

	o.err = o.loopWithError(ctx)
}

func (o *Runner) loopWithError(ctx context.Context) error {
	defer o.bhyvectlDestroyLastDitch(5*time.Second, "bhyve runner exit")

	o.status = bhyveStatusRunning

	// We track the context being marked as done using a separate
	// channel so that we can kick off a shutdown timer without
	// "select" always picking the Context's closed Done channel.
	go func() {
		<-ctx.Done()

		o.startShutdown(ctx.Err())
	}()

	const shutdownTimeout = time.Minute
	var shutdownTimedout <-chan time.Time

	var finalErr error

loop:
	select {
	case err := <-o.shutdownStarted:
		finalErr = err

		log.Printf("shutting down due to %v - waiting %s for bhyve to exit...",
			finalErr, shutdownTimeout.String())

		shutdownTimedout = time.After(shutdownTimeout)

		if o.status == bhyveStatusRunning {
			_ = o.acpiPowerOff()
		}
	case <-shutdownTimedout:
		log.Printf("pulling power cable because bhyve process did not exit after %s...",
			shutdownTimeout)

		_ = o.pullPowerCable()

		// Give the bhyve process one last chance to exit.
		select {
		case <-time.After(3 * time.Second):
		case <-o.bhyveExited:
		}

		return finalErr
	case req := <-o.statusRequests:
		req.status = o.status

		close(req.cb)
	case req := <-o.powerRequests:
		clientMsg, _ := o.handlePowerStateRequest(req)

		if clientMsg != "" {
			req.cb <- errors.New(clientMsg)
		} else {
			req.cb <- nil
		}

		close(req.cb)
	case exitedErr := <-o.bhyveExited:
		o.status = bhyveStatusExited

		err := o.isBhyveExitErrFatal(exitedErr)
		if err != nil {
			if finalErr != nil {
				return fmt.Errorf("bhyve exited - %w (caused by: %v)",
					err, finalErr)
			}

			return fmt.Errorf("bhyve exited - %w", err)
		}

		// A nil error indicates that the VM was rebooted,
		// so we just continue the loop.
		log.Println("restarting bhyve because bhyve exited with status 0 (vm rebooted)...")

		err = o.startBhyve(ctx)
		if err != nil {
			return fmt.Errorf("failed to start bhyve for vm reboot - %w",
				err)
		}

		o.status = bhyveStatusRunning
	}

	goto loop
}

func (o *Runner) handlePowerStateRequest(req *PowerStateRequest) (clientMsg string, err error) {
	log.Printf("received power state request - new state: %q", req.newState.String())

	const timeSpec = time.RFC822

	switch req.newState {
	case AcpiOffPowerState:
		o.status = fmt.Sprintf("shutting down... (acpi power button pressed at %s)",
			time.Now().Format(timeSpec))

		err := o.acpiPowerOff()
		if err != nil {
			return err.Error(), fmt.Errorf("failed to acpi power off - %w", err)
		}

		return "", nil
	case PullPowerCablePowerState:
		o.status = fmt.Sprintf("shutting down... (power cable unplugged at %s)",
			time.Now().Format(timeSpec))

		err := o.pullPowerCable()
		if err != nil {
			return err.Error(), fmt.Errorf("failed to unplug power cable - %w", err)
		}

		return "", nil
	default:
		log.Println("[warn] unknown power state type was requested")

		return "unknown power state type", nil
	}
}

func (o *Runner) acpiPowerOff() error {
	if !o.isBhyveRunning() {
		log.Println("[warn] acpi off requested, but bhyve is not running")

		return nil
	}

	log.Println("acpi powering off or killing bhyve...")

	// Trigger ACPI poweroff, refer to "man bhyve" for more info.
	err := o.bhyveProc.Process.Signal(syscall.SIGTERM)
	if err != nil {
		err = fmt.Errorf("failed to send sigterm to bhyve - %w", err)

		log.Printf("[warn] %s", err)

		return err
	}

	return nil
}

func (o *Runner) pullPowerCable() error {
	if !o.isBhyveRunning() {
		log.Println("[warn] unplug power cable requested, but bhyve is not running")

		return nil
	}

	log.Println("unplugging power cable from bhyve...")

	err := o.bhyveProc.Process.Signal(syscall.SIGKILL)
	if err != nil {
		err = fmt.Errorf("failed to send sigkill to bhyve - %w", err)

		log.Printf("[warn] %s", err)

		return err
	}

	return nil
}

func (o *Runner) bhyvectlDestroyLastDitch(timeout time.Duration, scenario string) {
	if !o.vmmDeviceExists() {
		return
	}

	log.Printf("destroying existing vm device due to %q...", scenario)

	ctx, cancelFn := context.WithTimeout(context.Background(), timeout)
	defer cancelFn()

	err := o.bhyvectlDestroy(ctx)
	if err != nil {
		log.Printf("[warn] failed to destroy vm device - %s", err)
		return
	}

	log.Println("successfully destroyed vm device")
}

func (o *Runner) bhyvectlDestroy(ctx context.Context) error {
	err := o.bhyvectl(ctx, "--destroy")
	if err != nil {
		return err
	}

	return nil
}

func (o *Runner) bhyvectl(ctx context.Context, arg string, args ...string) error {
	bhyvectl := exec.CommandContext(
		ctx,
		"/usr/sbin/bhyvectl",
		"--vm",
		o.config.VmName,
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

func (o *Runner) vmmDeviceExists() bool {
	_, statErr := os.Stat(filepath.Join("/dev/vmm", o.config.VmName))
	return statErr == nil
}

func (o *Runner) isBhyveRunning() bool {
	// Exec.Cmd.ProcessState is non-nil if the process has exited.
	return o.bhyveProc != nil && o.bhyveProc.ProcessState == nil
}

func (o *Runner) isBhyveExitErrFatal(exitedErr error) error {
	// Note: Refer to "man bhyve" for exit status info.

	if exitedErr == nil {
		// err == nil means exit status 0.
		return nil
	}

	var execExitErr *exec.ExitError

	if errors.As(exitedErr, &execExitErr) {
		switch execExitErr.ExitCode() {
		case 1:
			// Powered off.
			return fmt.Errorf("status 1 - vm was powered off")
		case 2:
			// Halted.
			return fmt.Errorf("status 2 - vm was halted")
		case 3:
			// Triple fault.
			return fmt.Errorf("status 3 - vm triple faulted")
		case 4:
			return fmt.Errorf("bhyve error (status 4) - %w - stderr: %s",
				exitedErr, o.stderr.String())
		default:
			return fmt.Errorf("unknown bhyve error (status %d) - %w - stderr: %s",
				execExitErr.ExitCode(), exitedErr, o.stderr.String())
		}
	}

	return fmt.Errorf("unable to get bhyve exit status - %s - stderr: %s",
		exitedErr, o.stderr.String())
}
