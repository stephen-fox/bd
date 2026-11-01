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

	appConfig, err := loadAppConfig()
	if err != nil {
		return fmt.Errorf("failed to load app config - %w", err)
	}

	vmRuntimeDir := vmRuntimeDirPath(vmName)

	_, gotResponse, _ := vmStatus(vmName, appConfig, false)

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

	return execStartDaemon([]string{vmName})
}
