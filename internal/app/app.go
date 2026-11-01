package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"

	"codeberg.org/stephen-fox/bd/internal/config"
)

const (
	appName = "bd"
	version = "v0.2.0-dev"

	usage = `SYNOPSIS
  ` + appName + ` -h
  ` + appName + ` version
  ` + appName + ` [OPTIONS] install [INSTALL-OPTIONS]
  ` + appName + ` [OPTIONS] new [NEW-OPTIONS] VM-NAME
  ` + appName + ` [OPTIONS] ls [LIST-OPTIONS] [VM-NAMES...]
  ` + appName + ` [OPTIONS] status [STATUS-OPTIONS] VM-NAME
  ` + appName + ` [OPTIONS] start [START-OPTIONS] VM-NAME
  ` + appName + ` [OPTIONS] restart [RESTART-OPTIONS] VM-NAME
  ` + appName + ` [OPTIONS] stop [STOP-OPTIONS] VM-NAME
  ` + appName + ` [OPTIONS] unplug [UNPLUG-OPTIONS] VM-NAME
  ` + appName + ` [OPTIONS] autostart [AUTOSTART-OPTIONS]
  ` + appName + ` [OPTIONS] console [CONSOLE-OPTIONS] VM-NAME
  ` + appName + ` [OPTIONS] delete [DELETE-OPTIONS] VM-NAME
  ` + appName + ` [OPTIONS] genmac [GENMAC-OPTIONS]

DESCRIPTION
  bd (bhyve daemon) is a bhyve virtual machine manager, similar to vm-bhyve.

OPTIONS
`

	singleVmModeArg = "M"
	dryRunModeArg   = "D"

	singleVmModeDesc = "Single machine mode: automatically pick the VM *if* only one exists"

	vmSocketsPerm = fs.FileMode(0o660)
)

// Various directory and configuration file paths.
const (
	appConfigDirPath  = "/usr/local/etc/" + appName
	appConfigFilePath = appConfigDirPath + "/" + appName + ".conf"

	appRuntimeDirPath          = "/usr/local/var/" + appName
	topLevelVmVariablesDirPath = appRuntimeDirPath + "/vm-variables"
	topLevelVmRuntimeDirPath   = appRuntimeDirPath + "/vm-runtime"
)

// RunApp is the equivalent to the "main" function. It resides in its
// own library to prevent bd's main.go from becoming cluttered.
func RunApp() error {
	displayHelp := flag.Bool("h", false, "Display this information")

	flag.Parse()

	if *displayHelp {
		_, _ = os.Stderr.WriteString(usage)
		flag.PrintDefaults()
		os.Exit(1)
	}

	if flag.NArg() == 0 {
		return errors.New("please specify a command as a non-flag argument or '-h' for more information")
	}

	flagSet := flag.NewFlagSet(flag.Arg(0), flag.ExitOnError)

	switch flag.Arg(0) {
	case "version":
		fmt.Println(version)
		return nil
	case "install":
		return doInstall(flagSet)
	case "genmac":
		return genmac()
	case "daemon":
		return daemon(flagSet)
	case "console-daemon":
		return consoleDaemon(flagSet)
	case "new":
		return newvm(flagSet)
	case "list", "ls":
		return list(flagSet)
	case "status":
		return status(flagSet)
	case "start":
		return start(flagSet)
	case "autostart":
		return autostart(flagSet)
	case "restart":
		return restart(flagSet)
	case "stop":
		return stop(flagSet)
	case "unplug":
		return unplug(flagSet)
	case "console":
		return console(flagSet)
	case "delete":
		return deletevm(flagSet)
	default:
		return fmt.Errorf("unknown command: '%s'", flag.Arg(0))
	}
}

func forEachVm(ctx context.Context, appConfig *config.AppConfig, fn func(ctx context.Context, vmName string) error) error {
	entries, err := os.ReadDir(appConfig.General.VmsStorageDir)
	if err != nil {
		return fmt.Errorf("failed to read virtual machines storage directory - %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Keep going.
		}

		vmName := entry.Name()

		err := fn(ctx, vmName)
		if err != nil {
			return err
		}
	}

	return nil
}

func lookupUser(username string) (uid uint32, gid uint32, err error) {
	account, err := user.Lookup(username)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to lookup user '%s' - %w", username, err)
	}

	uidI, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to parse uid - %w", err)
	}

	gidI, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to parse gid - %w", err)
	}

	return uint32(uidI), uint32(gidI), nil
}

func consoleSocketPath(runtimeDirPath string) string {
	return filepath.Join(runtimeDirPath, "console.sock")
}

func statusSocketPath(runtimeDirPath string) string {
	return filepath.Join(runtimeDirPath, "status.sock")
}

func powerStateSocketPath(runtimeDirPath string) string {
	return filepath.Join(runtimeDirPath, "power.sock")
}

func vmRuntimeDirPath(vmName string) string {
	return filepath.Join(topLevelVmRuntimeDirPath, vmName)
}

func vmVariablesDirPath(vmName string) string {
	return filepath.Join(topLevelVmVariablesDirPath, vmName)
}

func loadAppConfig() (*config.AppConfig, error) {
	_, err := os.Stat(appConfigDirPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat app config dir - make sure to run '%s init' first (%w)",
			appName, err)
	}

	f, err := os.Open(appConfigFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open app config file - %w", err)
	}
	defer f.Close()

	appConfig, err := config.ParseAppConfig(f)
	if err != nil {
		return nil, fmt.Errorf("failed to parse app config - %w", err)
	}

	return appConfig, nil
}

func loadVmConfig(vmName string, appConfig *config.AppConfig) (*config.VmConfig, error) {
	configPath := vmConfigFilePath(vmName, appConfig)

	configDir := filepath.Dir(configPath)

	_, err := os.Stat(configDir)
	if err != nil {
		return nil, fmt.Errorf("failed to stat vm config dir (does the vm exist?) - %w", err)
	}

	f, err := os.Open(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open vm config file - %w", err)
	}
	defer f.Close()

	vmConfig, err := config.ParseVmConfig(f, vmName)
	if err != nil {
		return nil, fmt.Errorf("failed to parse vm config - %w", err)
	}

	return vmConfig, nil
}

func vmConfigFilePath(vmName string, appConfig *config.AppConfig) string {
	return filepath.Join(appConfig.General.VmsStorageDir, vmName, "vm.conf")
}

func vmStorageDirPath(vmName string, appConfig *config.AppConfig) string {
	return filepath.Join(appConfig.General.VmsStorageDir, vmName)
}

func getOnlyOneNonFlagArg(flagSet *flag.FlagSet, lookingFor string) (string, error) {
	switch flagSet.NArg() {
	case 0:
		return "", fmt.Errorf("please specify only one non-flag argument: %s", lookingFor)
	case 1:
		return flagSet.Arg(0), nil
	default:
		return "", fmt.Errorf("please specify only one non-flag argument (got %d arguments)",
			flagSet.NArg())
	}
}

func getVmFromFlagsOrSingleVmMode(flagSet *flag.FlagSet, singleVmMode *bool) (string, error) {
	switch flagSet.NArg() {
	case 0:
		if !*singleVmMode {
			return "", errors.New("please specify a vm name as the first non-flag argument")
		}

		entries, err := os.ReadDir(topLevelVmRuntimeDirPath)
		if err != nil {
			return "", fmt.Errorf("failed to read vm runtime dir - %w", err)
		}

		if len(entries) != 1 {
			return "", fmt.Errorf("expected only one vm - found: %d",
				len(entries))
		}

		return entries[0].Name(), nil
	case 1:
		return flagSet.Arg(0), nil
	default:
		return "", fmt.Errorf("please specify only one non-flag argument (got %d arguments)",
			flagSet.NArg())
	}
}

func checkIfRunningAsRoot() error {
	uid := os.Getuid()
	gid := os.Getgid()

	switch {
	case uid != 0:
		return fmt.Errorf("this mode requires the program be run as root (current uid is: %d)", uid)
	case gid != 0:
		return fmt.Errorf("this mode requires the program be run as root (current gid is: %d)", gid)
	default:
		return nil
	}
}
