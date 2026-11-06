package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"

	"codeberg.org/stephen-fox/bd/internal/config"
)

func status(flagSet *flag.FlagSet) error {
	// doNotFailIfDaemonIsStopped := flagSet.Bool(
	// 	"",
	// 	false,
	// 	"")

	singleVmMode := flagSet.Bool(
		singleVmModeArg,
		false,
		singleVmModeDesc)

	dumpConfigMode := flagSet.Bool(
		"I",
		false,
		"Dump the VM's configuration to stdout as JSON")

	verbose := flagSet.Bool(
		"v",
		false,
		"Display additional information about the status of the VM")

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

	if *dumpConfigMode {
		vmConfig, err := loadVmConfig(vmName, appConfig)
		if err != nil {
			return err
		}

		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")

		return encoder.Encode(vmConfig)
	}

	status := statusGetter{
		verbose: *verbose,
	}

	status.Get(vmName, appConfig)

	fmt.Print(status.String())

	if status.oneSuccess {
		return nil
	} else {
		os.Exit(1)
	}

	return nil
}

type statusGetter struct {
	statuses       []vmStatusInfo
	longestNameLen int
	verbose        bool
	oneSuccess     bool
}

type vmStatusInfo struct {
	name   string
	status string
}

func (o *statusGetter) Get(vmName string, appConfig *config.AppConfig) {
	o.statuses = append(o.statuses, vmStatusInfo{
		name: vmName,
	})

	if len(vmName) > o.longestNameLen {
		o.longestNameLen = len(vmName)
	}

	status, _, err := vmStatus(vmName, appConfig, o.verbose)
	if err != nil {
		o.statuses[len(o.statuses)-1].status = "unknown (" + err.Error() + ")"
	} else {
		o.statuses[len(o.statuses)-1].status = status

		o.oneSuccess = true
	}
}

func (o *statusGetter) NumStatuses() int {
	return len(o.statuses)
}

func (o *statusGetter) String() string {
	if len(o.statuses) == 0 {
		return ""
	}

	sort.SliceStable(o.statuses, func(i, j int) bool {
		return o.statuses[i].name < o.statuses[j].name
	})

	var buf bytes.Buffer

	const minSpaces = 1

	for _, status := range o.statuses {
		numSpaces := minSpaces

		if len(status.name) < o.longestNameLen {
			numSpaces = o.longestNameLen - len(status.name) + minSpaces
		}

		buf.WriteString(status.name)

		buf.WriteString(strings.Repeat(" ", numSpaces))

		buf.WriteString(status.status)

		buf.WriteByte('\n')
	}

	return buf.String()
}

func vmStatus(vmName string, appConfig *config.AppConfig, verbose bool) (string, bool, error) {
	configFilePath := vmConfigFilePath(vmName, appConfig)

	_, statErr := os.Stat(configFilePath)
	switch {
	case statErr == nil:
		// Keep going.
	case errors.Is(statErr, os.ErrNotExist):
		return "", false, fmt.Errorf("no such vm: %q", vmName)
	default:
		return "", false, statErr
	}

	vmStatusSocketPath := statusSocketPath(vmRuntimeDirPath(vmName))

	conn, err := net.Dial("unix", vmStatusSocketPath)
	if err != nil {
		if verbose {
			return fmt.Sprintf("stopped (%s)", err), false, nil
		}

		return "stopped", false, nil
	}
	defer conn.Close()

	scanner := bufio.NewScanner(conn)

	if !scanner.Scan() {
		return "", false, fmt.Errorf("failed to read from socket - %w", scanner.Err())
	}

	return scanner.Text(), true, nil
}
