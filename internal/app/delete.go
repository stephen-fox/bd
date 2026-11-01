package app

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func deletevm(flagSet *flag.FlagSet) error {
	_ = flagSet.Parse(os.Args[2:])

	err := checkIfRunningAsRoot()
	if err != nil {
		return err
	}

	switch flagSet.NArg() {
	case 0:
		return fmt.Errorf("please specify the name of the vm to delete as a non-flag argument")
	case 1:
		// OK.
	default:
		return fmt.Errorf("please specify only one non-flag argument (the vm to delete)")
	}

	appConfig, err := loadAppConfig()
	if err != nil {
		return fmt.Errorf("failed to load app config - %w", err)
	}

	vmName := flagSet.Arg(0)

	status, _, err := vmStatus(vmName, appConfig, false)
	if err != nil {
		return fmt.Errorf("failed to get vm status - %w", err)
	}

	if status != "stopped" {
		return fmt.Errorf("please stop the vm before deleting it (current status is %q)",
			status)
	}

	os.Stderr.WriteString("WARNING - this will delete vm '" + vmName +
		"' - are you sure? (input the vm name and press enter to start deletion)\n")

	scanner := bufio.NewScanner(os.Stdin)

	scanner.Scan()

	if scanner.Err() != nil {
		return fmt.Errorf("failed to read from stdin - %w", scanner.Err())
	}

	if scanner.Text() != vmName {
		return fmt.Errorf("the vm name you entered does not match - skipping deletion")
	}

	zfsId := strings.TrimPrefix(appConfig.General.VmsStorageDir+"/"+vmName, "/")

	zfsDestroy := exec.Command("/sbin/zfs", "destroy", zfsId)

	out, err := zfsDestroy.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to destroy vm's zfs dataset at %q - %w (output: %s)",
			zfsId, err, out)
	}

	runtimeDir := filepath.Clean(vmRuntimeDirPath(vmName))

	if !strings.HasPrefix(runtimeDir, topLevelVmRuntimeDirPath) || runtimeDir == topLevelVmRuntimeDirPath {
		return fmt.Errorf("warning: vm runtime dir was set to %q - "+
			"refusing to delete it, this is a bug!",
			runtimeDir)
	}

	_ = os.RemoveAll(runtimeDir)

	variablesDir := filepath.Clean(vmVariablesDirPath(vmName))

	if !strings.HasPrefix(variablesDir, topLevelVmVariablesDirPath) || variablesDir == topLevelVmVariablesDirPath {
		return fmt.Errorf("warning: vm variables dir was set to %q - "+
			"refusing to delete it, this is a bug!",
			variablesDir)
	}

	_ = os.RemoveAll(variablesDir)

	os.Stderr.WriteString("'" + vmName + "' deleted\n")

	return nil
}
