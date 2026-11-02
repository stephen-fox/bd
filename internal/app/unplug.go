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

	os.Stderr.WriteString("WARNING - this will effectively unplug the power from '" +
		vmName + "' which may result in data loss - are you sure? " +
		"(type the vm name and press enter to unplug it)\n> ")

	stdinScanner := bufio.NewScanner(os.Stdin)

	stdinScanner.Scan()

	if stdinScanner.Err() != nil {
		return fmt.Errorf("failed to read from stdin - %w", stdinScanner.Err())
	}

	if stdinScanner.Text() != vmName {
		return fmt.Errorf("the vm name you entered does not match - skipping unplug")
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

	powerScanner := bufio.NewScanner(conn)

	if !powerScanner.Scan() {
		return powerScanner.Err()
	}

	errMsg := powerScanner.Text()
	if errMsg != "" {
		return errors.New(errMsg)
	}

	return nil
}
