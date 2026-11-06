package app

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"

	"codeberg.org/stephen-fox/bd/internal/bhyver"
)

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

	return stopVm(vmName)
}

func stopVm(vmName string) error {
	vmStatusSocketPath := statusSocketPath(vmRuntimeDirPath(vmName))

	statusConn, err := net.Dial("unix", vmStatusSocketPath)
	if err != nil {
		return fmt.Errorf("failed to open status unix socket (is the vm currently running?) - %w", err)
	}
	defer statusConn.Close()

	statusScanner := bufio.NewScanner(statusConn)

	vmRuntimeDir := vmRuntimeDirPath(vmName)

	powerConn, err := net.Dial("unix", powerStateSocketPath(vmRuntimeDir))
	if err != nil {
		return fmt.Errorf("failed to open power state unix socket (is the vm currently running?) - %w", err)
	}
	defer powerConn.Close()

	_, err = powerConn.Write([]byte(bhyver.AcpiOffPowerState.String() + "\n"))
	if err != nil {
		return err
	}

	powerScanner := bufio.NewScanner(powerConn)

	if !powerScanner.Scan() {
		return powerScanner.Err()
	}

	errMsg := powerScanner.Text()
	if errMsg != "" {
		return errors.New(errMsg)
	}

	for statusScanner.Scan() {
		status := statusScanner.Text()

		switch {
		case strings.HasPrefix(status, "error: "):
			return errors.New(status)
		case status == "stopped":
			return nil
		}
	}

	return nil
}
