package app

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"codeberg.org/stephen-fox/bd/internal/config"
	"codeberg.org/stephen-fox/bd/internal/osmaybe"
)

func newvm(flagSet *flag.FlagSet) error {
	sizeGb := flagSet.Uint64("s", 100, "VM disk size in gb")

	dryRun := flagSet.Bool(dryRunModeArg, false, "Run in dry-run mode; logs operations but does not actually create a vm")

	_ = flagSet.Parse(os.Args[2:])

	if !*dryRun {
		err := checkIfRunningAsRoot()
		if err != nil {
			return err
		}
	}

	vmName, err := getOnlyOneNonFlagArg(flagSet, "the vm name of the vm create")
	if err != nil {
		return err
	}

	switch {
	case strings.Contains(vmName, " "):
		return fmt.Errorf("vm name cannot contain spaces")
	case strings.Contains(vmName, "/"):
		return fmt.Errorf("vm name cannot contain slashes")
	}

	testPath := filepath.Join("/", "test", "example", vmName)
	expected := "/test/example/" + vmName

	if testPath != expected {
		return fmt.Errorf("vm name contains disallowed characters - expected test path to be: %q - got %q",
			expected, testPath)
	}

	appConfig, err := loadAppConfig()
	if err != nil {
		return err
	}

	maybeDo := osmaybe.MaybeDo{
		DryRunMode: *dryRun,
	}

	if maybeDo.DryRunMode {
		maybeDo.Logger = log.New(log.Default().Writer(), "[dry-run] ", log.Default().Flags())
	}

	var undo undoFns

	vmVariablesDir := vmVariablesDirPath(vmName)

	err = maybeDo.Mkdir(vmVariablesDir, 0o700)
	if err != nil {
		return fmt.Errorf("failed to create vm variables dir: %q - %w",
			vmVariablesDir, err)
	}

	undo = append(undo, func() { maybeDo.Remove(vmVariablesDir) })

	vmStorageDir := vmStorageDirPath(vmName, appConfig)

	vmStorageZfsId := strings.TrimLeft(vmStorageDir, "/")

	zfsCreate := exec.Command("/sbin/zfs", "create", vmStorageZfsId)

	output, err := maybeDo.ExecCombinedOutput(zfsCreate)
	if err != nil {
		undo.undoFromEnd()

		return fmt.Errorf("failed to create zfs dataset for vm storage: %q - %w (output: %q)",
			vmStorageZfsId, err, output)
	}

	undo = append(undo, func() { maybeDo.ExecCombinedOutput(exec.Command("/sbin/zfs", "destroy", vmStorageZfsId)) })

	undo = append(undo, func() { maybeDo.Remove(vmStorageDir) })

	err = maybeDo.Chmod(vmStorageDir, 0o700)
	if err != nil {
		undo.undoFromEnd()

		return fmt.Errorf("failed to chmod vm storage dir: %q - %w",
			vmStorageDir, err)
	}

	vmConfigPath := vmConfigFilePath(vmName, appConfig)
	vmConfigDir := filepath.Dir(vmConfigPath)

	err = maybeDo.MkdirAll(vmConfigDir, 0o755)
	if err != nil {
		undo.undoFromEnd()

		return fmt.Errorf("failed to create vm config dir: %q - %w",
			vmConfigDir, err)
	}

	undo = append(undo, func() { maybeDo.Remove(vmConfigDir) })

	err = maybeDo.Chmod(vmConfigDir, 0o700)
	if err != nil {
		undo.undoFromEnd()

		return fmt.Errorf("failed to chmod vm config dir: %q - %w",
			vmConfigDir, err)
	}

	var buf bytes.Buffer

	err = config.NewVmConfigFile(&buf)
	if err != nil {
		undo.undoFromEnd()

		return fmt.Errorf("failed to generate vm config file - %w",
			err)
	}

	err = maybeDo.WriteFileHumanReadable(vmConfigPath, buf.Bytes(), 0o644)
	if err != nil {
		undo.undoFromEnd()

		return fmt.Errorf("failed to write vm config file: %q - %w",
			vmConfigPath, err)
	}

	undo = append(undo, func() { maybeDo.Remove(vmConfigPath) })

	err = maybeDo.Chmod(vmConfigDir, 0o700)
	if err != nil {
		undo.undoFromEnd()

		return fmt.Errorf("failed to chmod vm storage dir: %q - %w",
			vmStorageDir, err)
	}

	vmDiskPath := filepath.Join(vmStorageDir, "disk0.img")

	truncate := exec.Command("/usr/bin/truncate",
		"-s", fmt.Sprintf("%dG", *sizeGb),
		vmDiskPath)

	truncateOutput, err := maybeDo.ExecCombinedOutput(truncate)
	if err != nil {
		undo.undoFromEnd()

		return fmt.Errorf("failed to create vm disk: %q - %w - (truncate output: %s)",
			vmDiskPath, err, truncateOutput)
	}

	undo = append(undo, func() { maybeDo.Remove(vmDiskPath) })

	origUefiVarsFileInfo, err := os.Stat("/usr/local/share/uefi-firmware/BHYVE_UEFI_VARS.fd")
	if err == nil && !origUefiVarsFileInfo.IsDir() {
		uefiVarsFilePath := filepath.Join(vmStorageDir, config.UefiVarsFileName)

		output, err := maybeDo.ExecCombinedOutput(exec.Command(
			"/bin/cp",
			"/usr/local/share/uefi-firmware/BHYVE_UEFI_VARS.fd",
			uefiVarsFilePath))
		if err != nil {
			return fmt.Errorf("failed to copy uefi variables file to vm storage dir - %w - output: %q",
				err, output)
		}

		undo = append(undo, func() { maybeDo.Remove(uefiVarsFilePath) })
	}

	os.Stderr.WriteString("vm config file can be found at: ")
	os.Stdout.WriteString(vmConfigPath + "\n")

	return nil
}

type undoFns []func()

func (o undoFns) undoFromEnd() {
	if len(o) == 0 {
		return
	}

	for i := len(o) - 1; i > -1; i-- {
		o[i]()
	}
}
