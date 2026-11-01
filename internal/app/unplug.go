package app

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"

	"codeberg.org/stephen-fox/bd/internal/bhyver"
)

func unplug(flagSet *flag.FlagSet) error {
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
