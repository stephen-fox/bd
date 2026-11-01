package app

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

func autostart(flagSet *flag.FlagSet) error {
	_ = flagSet.Parse(os.Args[2:])

	err := checkIfRunningAsRoot()
	if err != nil {
		return err
	}

	appConfig, err := loadAppConfig()
	if err != nil {
		return fmt.Errorf("failed to load app config - %w", err)
	}

	var autostartableVms []string
	var failedVms []string

	err = forEachVm(context.Background(), appConfig, func(_ context.Context, vmName string) error {
		vmConfig, err := loadVmConfig(vmName, appConfig)
		if err != nil {
			failedVms = append(failedVms, vmName)
			log.Printf("autostart: %q - failed to load config - %s", vmName, err)

			return nil
		}

		if !vmConfig.General.Autostart {
			return nil
		}

		_, gotResponse, _ := vmStatus(vmName, appConfig, false)
		if gotResponse {
			log.Printf("autostart: %q - skipping because vm is already running", vmName)

			return nil
		}

		autostartableVms = append(autostartableVms, vmName)

		log.Printf("autostart: %q - starting daemon...", vmName)

		err = execDaemon([]string{vmName})
		if err != nil {
			failedVms = append(failedVms, vmName)
			log.Printf("autostart: %q - failed to exec daemon - %s", vmName, err)

			return nil
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("autostart failed - %w", err)
	}

	if len(failedVms) > 0 {
		return fmt.Errorf("failed to autostart %d/%d auto-startable virtual machine(s): %s",
			len(failedVms), len(autostartableVms), `"`+strings.Join(failedVms, `" `)+`"`)
	}

	if len(autostartableVms) == 0 {
		log.Println("no virtual machines were autostarted")
	} else {
		log.Printf("autostarted %d virtual machine(s): %s",
			len(autostartableVms), `"`+strings.Join(autostartableVms, `" `)+`"`)
	}

	return nil
}
