package app

import (
	"context"
	"flag"
	"fmt"
	"os"
)

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

	appConfig, err := loadAppConfig()
	if err != nil {
		return fmt.Errorf("failed to load app config - %w", err)
	}

	var status statusGetter

	if flagSet.NArg() == 0 {
		err := forEachVm(context.Background(), appConfig, func(_ context.Context, vmName string) error {
			status.Get(vmName, appConfig)

			return nil
		})
		if err != nil {
			return err
		}
	} else {
		for _, vmName := range flag.Args() {
			status.Get(vmName, appConfig)
		}
	}

	if status.NumStatuses() == 0 {
		return nil
	}

	fmt.Print(status.String())

	if !status.oneSuccess {
		os.Exit(1)
	}

	return nil
}
