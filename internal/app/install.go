package app

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	"codeberg.org/stephen-fox/bd/internal/config"
)

func doInstall(flagSet *flag.FlagSet) error {
	_ = flagSet.Parse(os.Args[2:])

	err := checkIfRunningAsRoot()
	if err != nil {
		return err
	}

	err = os.MkdirAll(appConfigDirPath, 0o755)
	if err != nil {
		return fmt.Errorf("failed to create application config directory - %w", err)
	}

	_, err = os.Stat(appConfigFilePath)
	if err == nil {
		return fmt.Errorf("an application-level config file already exists at: %q",
			appConfigFilePath)
	}

	var buf bytes.Buffer

	err = config.NewAppConfigFile(&buf)
	if err != nil {
		return fmt.Errorf("failed to generate application-level config - %w", err)
	}

	err = os.WriteFile(appConfigFilePath, buf.Bytes(), 0o600)
	if err != nil {
		return fmt.Errorf("failed to create application-level config-file - %w", err)
	}

	err = os.MkdirAll(topLevelVmVariablesDirPath, 0o700)
	if err != nil {
		return fmt.Errorf("failed to create variables directory - %w", err)
	}

	os.Stderr.WriteString("created configuration file at: ")
	fmt.Println(appConfigFilePath)
	os.Stderr.WriteString("make sure to update its default values\n")

	return nil
}
