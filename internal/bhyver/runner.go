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
	"syscall"
	"time"

	"gitlab.com/stephen-fox/bd/internal/passfd"
)

// StartRunner instantiates a Runner and starts it.
func StartRunner(ctx context.Context, vmName string, bhyveArgs []string, powerRequests <-chan PowerStateRequest, optConsoleFdConn *net.UnixConn) *Runner {
	runner := &Runner{
		vmName:    vmName,
		bhyveArgs: bhyveArgs,
		consoled:  optConsoleFdConn,
		powerReqs: powerRequests,
		exited:    make(chan error, 1),
		stderr:    bytes.NewBuffer(nil),
		done:      make(chan struct{}),
	}

	go runner.loop(ctx)

	return runner
}

// Runner operates a bhyve process.
type Runner struct {
	vmName    string
	bhyveArgs []string
	consoled  *net.UnixConn
	powerReqs <-chan PowerStateRequest
	exited    chan error
	execCmd   *exec.Cmd
	stderr    *bytes.Buffer
	done      chan struct{}
	err       error
}

func (o *Runner) Done() <-chan struct{} {
	return o.done
}

func (o *Runner) Err() error {
	return o.err
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
	err := o.start(ctx)
	if err != nil {
		return fmt.Errorf("failed to start bhyve for the first time - %w - stderr: %q",
			err, o.stderr.String())
	}
	defer o.bhyvectlDestroyLastDitch(5*time.Second, "runner exit")

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

func (o *Runner) onPowerStateRequest(ctx context.Context, newState PowerState) (clientMsg string, err error) {
	log.Printf("received power state request - new state: %q", newState.String())

	switch newState {
	case OnPowerState:
		err := o.start(ctx)
		if err != nil {
			return err.Error(), fmt.Errorf("failed to start vm - %w", err)
		}

		return "", nil
	case AcpiOffPowerState:
		err := o.acpiOffOrKill(ctx)
		if err != nil {
			return err.Error(), fmt.Errorf("failed to acpi power off - %w", err)
		}

		return "", nil
	case AcpiRebootPowerState:
		err := o.acpiOffOrKill(ctx)
		if err != nil {
			return err.Error(), fmt.Errorf("failed to acpi power off for reboot - %w", err)
		}

		err = o.start(ctx)
		if err != nil {
			return err.Error(), fmt.Errorf("failed to start for reboot - %w", err)
		}

		return "", nil
	case PullPowerCablePowerState:
		err := o.pullPowerCable(ctx)
		if err != nil {
			return err.Error(), fmt.Errorf("failed to pull power cable - %w", err)
		}

		return "", nil
	case PullPowerCableRebootPowerState:
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

func (o *Runner) start(ctx context.Context) error {
	if o.isRunning() {
		log.Printf("[warn] - bhyve start was attempted, but process is already running")

		return nil
	}

	log.Println("starting bhyve...")

	if o.vmmDeviceExists() {
		err := o.bhyvectlDestroy(ctx)
		if err != nil {
			return fmt.Errorf("failed to destroy existing vm device on startup - %w", err)
		}
	}

	o.stderr.Reset()

	bhyve := exec.Command("/usr/sbin/bhyve", o.bhyveArgs...)

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

	if o.consoled != nil {
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

		err = passfd.Put(o.consoled, stdinFile, stdoutFile)
		if err != nil {
			return fmt.Errorf("failed to send console fds to console daemon - %w", err)
		}
	}

	log.Printf("exec'ing bhyve with argv: %q...", bhyve.String())

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

func (o *Runner) acpiOffOrKill(ctx context.Context) error {
	if !o.isRunning() {
		log.Println("[warn] acpi off requested, but bhyve is not running")

		return nil
	}

	log.Println("acpi powering off or killing bhyve...")

	// Trigger ACPI poweroff, refer to "man bhyve" for more info.
	err := o.execCmd.Process.Signal(syscall.SIGTERM)
	if err != nil {
		log.Printf("[warn] failed to send sigterm to bhyve - %s", err)
	}

	select {
	case <-ctx.Done():
		pullPowerCtx, cancelFn := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelFn()

		_ = o.pullPowerCable(pullPowerCtx)

		return ctx.Err()
	case err = <-o.exited:
		log.Printf("bhyve process exited after sending sigterm (child err: %v)", err)

		o.bhyvectlDestroyLastDitch(5*time.Second, "acpi power off")

		return nil
	}
}

func (o *Runner) pullPowerCable(ctx context.Context) error {
	if !o.isRunning() {
		log.Println("[warn] pull power cable requested, but bhyve is not running")

		return nil
	}
	defer o.bhyvectlDestroyLastDitch(5*time.Second, "pull power cable")

	log.Println("pulling power cable from bhyve...")

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

func (o *Runner) vmmDeviceExists() bool {
	_, statErr := os.Stat(filepath.Join("/dev/vmm", o.vmName))
	return statErr == nil
}

func (o *Runner) isRunning() bool {
	// Exec.Cmd.ProcessState is non-nil if the process has exited.
	return o.execCmd != nil && o.execCmd.ProcessState == nil
}

func (o *Runner) onExecCmdExit(ctx context.Context, exitedErr error) error {
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
