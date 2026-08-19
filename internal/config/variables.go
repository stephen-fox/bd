package config

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Variable prefixes / namespaces.
const (
	configVarsPrefix            = "BD_CONFIG"
	prestartDepConfigVarsPrefix = configVarsPrefix + "_PRESTART_DEP"
)

// Builtin variable names.
const (
	bootMediaDirVarName = configVarsPrefix + "_BOOT_MEDIA_DIR"
	storageDirVarName   = configVarsPrefix + "_STORAGE_DIR"
	vmNameVarName       = configVarsPrefix + "_VM_NAME"
	vmStorageDirVarName = configVarsPrefix + "_VM_STORAGE_DIR"
)

type LoadVariablesArgs struct {
	VmName             string
	VmVariablesDirPath string
	AppConfig          *AppConfig
	SkipLoading        []VariableType
}

func LoadVariables(args LoadVariablesArgs) (*Variables, error) {
	dirEntries, err := os.ReadDir(args.VmVariablesDirPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read vm context directory - %w", err)
	}

	vars := &Variables{
		variablesDirPath: args.VmVariablesDirPath,
		varNamesToData:   make(map[string]variableData),
	}

	err = vars.AddVariable(SystemVariableType, bootMediaDirVarName, args.AppConfig.General.BootMediaDir)
	if err != nil {
		return nil, fmt.Errorf("failed to add boot media variable - %w", err)
	}

	err = vars.AddVariable(SystemVariableType, storageDirVarName, args.AppConfig.General.VmsStorageDir)
	if err != nil {
		return nil, fmt.Errorf("failed to add vms storage dir variable - %w", err)
	}

	err = vars.AddVariable(SystemVariableType, vmNameVarName, args.VmName)
	if err != nil {
		return nil, fmt.Errorf("failed to add vm name variable - %w", err)
	}

	err = vars.AddVariable(SystemVariableType, vmStorageDirVarName, filepath.Join(args.AppConfig.General.VmsStorageDir, args.VmName))
	if err != nil {
		return nil, fmt.Errorf("failed to add vm-specific storage dir variable - %w", err)
	}

nextDir:
	for _, entry := range dirEntries {
		if !entry.IsDir() {
			continue
		}

		varTypeDirName := entry.Name()
		varType := VariableType(varTypeDirName)

		for _, skip := range args.SkipLoading {
			if skip == varType {
				continue nextDir
			}
		}

		switch varType {
		case PrestartDepVariableType:
			// Keep going.
		default:
			continue
		}

		varTypeDirPath := filepath.Join(args.VmVariablesDirPath, varTypeDirName)

		subDirEntries, err := os.ReadDir(varTypeDirPath)
		if err != nil {
			return nil, fmt.Errorf("%q: failed to read entries - %w", varTypeDirName, err)
		}

		for _, subDirEntry := range subDirEntries {
			if subDirEntry.IsDir() {
				continue
			}

			varName, err := base64.URLEncoding.DecodeString(subDirEntry.Name())
			if err != nil {
				return nil, fmt.Errorf("%q: failed to base64-url-decode variable file name: %q - %w",
					varTypeDirName, subDirEntry.Name(), err)
			}

			filePath := filepath.Join(varTypeDirPath, subDirEntry.Name())

			contents, err := os.ReadFile(filePath)
			if err != nil {
				return nil, fmt.Errorf("%q: failed to read contents of variable file: %q - %w",
					varTypeDirName, subDirEntry.Name(), err)
			}

			err = vars.AddVariable(VariableType(subDirEntry.Name()), string(varName), string(contents))
			if err != nil {
				return nil, fmt.Errorf("%q: failed to add variable %q - %w",
					varTypeDirName, varName, err)
			}
		}
	}

	return vars, nil
}

type Variables struct {
	variablesDirPath string
	varNamesToData   map[string]variableData
}

type variableData struct {
	vType VariableType
	value string
}

const (
	SystemVariableType      VariableType = "system"
	PrestartDepVariableType VariableType = "prestart-dep"
)

type VariableType string

func (o *Variables) AddVariable(kind VariableType, varName string, value string) error {
	existing, alreadyDefined := o.varNamesToData[varName]
	if alreadyDefined {
		return fmt.Errorf("config parameter already defined as %q type: %q",
			existing.vType, varName)
	}

	o.varNamesToData[varName] = variableData{
		vType: kind,
		value: value,
	}

	return nil
}

func (o *Variables) ReplaceVars(dataWithVars string) (string, error) {
	var unknownVarNames []string

	updated := os.Expand(dataWithVars, func(varName string) string {
		if !strings.HasPrefix(varName, configVarsPrefix+"_") {
			return "${" + varName + "}"
		}

		data, hasIt := o.varNamesToData[varName]
		if hasIt {
			return data.value
		}

		unknownVarNames = append(unknownVarNames, `"`+varName+`"`)

		return ""
	})

	if len(unknownVarNames) > 0 {
		return "", fmt.Errorf("failed to find values for the following variables: %s",
			strings.Join(unknownVarNames, ", "))
	}

	return updated, nil
}

func (o *Variables) SaveVariableToFs(ctx context.Context) error {
	switch {
	case o.variablesDirPath == "":
		return fmt.Errorf("vm context directory path is empty")
	case !filepath.IsAbs(o.variablesDirPath):
		return fmt.Errorf("vm variables directory path is not absolute: %q",
			o.variablesDirPath)
	}

	for varName, data := range o.varNamesToData {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// Keep going.
		}

		switch data.vType {
		case SystemVariableType:
			continue
		case PrestartDepVariableType:
			// Keep going.
		default:
			return fmt.Errorf("unsupported variable type: %q", data.vType)
		}

		fileName := base64.URLEncoding.EncodeToString([]byte(varName))
		filePath := filepath.Join(o.variablesDirPath, string(data.vType), fileName)

		err := os.MkdirAll(filepath.Dir(filePath), 0o700)
		if err != nil {
			return fmt.Errorf("failed to create variable-type directory for %q - %w",
				data.vType, err)
		}

		err = os.WriteFile(filePath, []byte(data.value), 0o700)
		if err != nil {
			return fmt.Errorf("failed to create file for variable %q - %w",
				varName, err)
		}
	}

	return nil
}
