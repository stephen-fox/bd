package app

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
)

func stopall(flagSet *flag.FlagSet) error {
	_ = flagSet.Parse(os.Args[2:])

	err := checkIfRunningAsRoot()
	if err != nil {
		return err
	}

	appConfig, err := loadAppConfig()
	if err != nil {
		return fmt.Errorf("failed to load app config - %w", err)
	}

	os.Stderr.WriteString("WARNING - are you sure you would like to stop all running vms?" +
		" (type 'yes' and press enter to shut down all vms)\n> ")

	stdinScanner := bufio.NewScanner(os.Stdin)

	stdinScanner.Scan()

	if stdinScanner.Err() != nil {
		return fmt.Errorf("failed to read from stdin - %w", stdinScanner.Err())
	}

	if stdinScanner.Text() != "yes" {
		return fmt.Errorf("skipped shutting down all vms")
	}

	workerPool := make(chan struct{}, 4)

	for i := 0; i < cap(workerPool); i++ {
		workerPool <- struct{}{}
	}

	var vmsToStop []string

	err = forEachVm(context.Background(), appConfig, func(_ context.Context, vmName string) error {
		status, _, _ := vmStatus(vmName, appConfig, false)
		if status == "running" {
			vmsToStop = append(vmsToStop, vmName)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("stopall failed - %w", err)
	}

	if len(vmsToStop) == 0 {
		return nil
	}

	onStop := make(chan bool, len(vmsToStop))

	for _, vmName := range vmsToStop {
		vmName := vmName

		go func() {
			<-workerPool

			log.Printf("stopping: %q...", vmName)

			err := stopVm(vmName)
			if err != nil {
				log.Printf("failed to stop %q - %s", vmName, err)

				onStop <- false
			} else {
				log.Printf("%q stopped", vmName)

				onStop <- true
			}

			workerPool <- struct{}{}
		}()
	}

	var numSucceeded int
	var numFailed int

	for succeeded := range onStop {
		if succeeded {
			numSucceeded++
		} else {
			numFailed++
		}

		if numSucceeded+numFailed == len(vmsToStop) {
			break
		}
	}

	if numFailed > 0 {
		return fmt.Errorf("failed to stop %d out of %d vms",
			numFailed, len(vmsToStop))
	}

	return nil
}
