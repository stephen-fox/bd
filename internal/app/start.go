package app

import (
	"flag"
	"fmt"
	"os"
)

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

	appConfig, err := loadAppConfig()
	if err != nil {
		return fmt.Errorf("failed to load app config - %w", err)
	}

	vmName, err := getVmFromFlagsOrSingleVmMode(flagSet, singleVmMode)
	if err != nil {
		return err
	}

	_, gotResponse, _ := vmStatus(vmName, appConfig, false)
	if gotResponse {
		return fmt.Errorf("vm is already running")
	}

	return execDaemon([]string{vmName})
}
