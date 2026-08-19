package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gitlab.com/stephen-fox/bd/internal/ini"
)

// AppConfig::General section and its parameters
const (
	GeneralAppConfigSection = "General"

	VmsStorageDirParam = "VmsStorageDir"
	BootMediaDirParam  = "BootMediaDir"
)

// ParseAppConfig parses an AppConfig from an io.Reader.
func ParseAppConfig(r io.Reader) (*AppConfig, error) {
	config := &AppConfig{}

	err := ini.ParseSchema(r, config)
	if err != nil {
		return nil, err
	}

	return config, nil
}

// AppConfig represents the application's configuration.
type AppConfig struct {
	General *General
}

// Rules partly implements the ini.Schema interface.
func (o *AppConfig) Rules() ini.ParserRules {
	return ini.ParserRules{
		RequiredSections: []string{
			GeneralAppConfigSection,
		},
	}
}

// OnGlobalParam partly implements the ini.Schema interface.
func (o *AppConfig) OnGlobalParam(paramName string) (func(*ini.Param) error, ini.SchemaRule) {
	return nil, ini.SchemaRule{}
}

// OnSection partly implements the ini.Schema interface.
func (o *AppConfig) OnSection(name string, _ string) (func() (ini.SectionSchema, error), ini.SchemaRule) {
	switch name {
	case "General":
		return func() (ini.SectionSchema, error) {
			o.General = &General{}

			return o.General, nil
		}, ini.SchemaRule{Limit: 1}
	default:
		return nil, ini.SchemaRule{}
	}
}

// Validate partly implements the ini.Schema interface.
func (o *AppConfig) Validate() error {
	return nil
}

type General struct {
	VmsStorageDir string
	BootMediaDir  string
}

// RequiredParams partly implements the ini.SectionSchema interface.
func (o *General) RequiredParams() []string {
	return nil
}

// OnParam partly implements the ini.SectionSchema interface.
func (o *General) OnParam(paramName string) (func(*ini.Param) error, ini.SchemaRule) {
	switch paramName {
	case VmsStorageDirParam:
		return func(p *ini.Param) error {
			o.VmsStorageDir = p.Value

			return nil
		}, ini.SchemaRule{Limit: 1}
	case BootMediaDirParam:
		return func(p *ini.Param) error {
			o.BootMediaDir = p.Value

			return nil
		}, ini.SchemaRule{Limit: 1}
	default:
		return nil, ini.SchemaRule{}
	}
}

// Validate partly implements the ini.SectionSchema interface.
func (o *General) Validate() error {
	err := dirPathIsAbsAndExists(o.VmsStorageDir)
	if err != nil {
		return fmt.Errorf("%q - %w", VmsStorageDirParam, err)
	}

	err = dirPathIsAbsAndExists(o.BootMediaDir)
	if err != nil {
		return fmt.Errorf("%q - %w", BootMediaDirParam, err)
	}

	return nil
}

func dirPathIsAbsAndExists(dirPath string) error {
	if strings.TrimSpace(dirPath) == "" {
		return fmt.Errorf("directory path is empty string")
	}

	if !filepath.IsAbs(dirPath) {
		return fmt.Errorf("directory path is not absolute; %q", dirPath)
	}

	info, err := os.Stat(dirPath)
	if err != nil {
		return fmt.Errorf("failed to stat: %q - %w", dirPath, err)
	}

	if !info.IsDir() {
		return fmt.Errorf("path is not a directory: %q", dirPath)
	}

	return nil
}

func NewAppConfigFile(w io.Writer) error {
	// Using a separate variable for the config contents so we can preview
	// it in the editor.
	const contents = `[` + GeneralAppConfigSection + `]
` + BootMediaDirParam + ` = /zroot/os-images
` + VmsStorageDirParam + ` = /zroot/vms
`
	_, err := w.Write([]byte(contents))

	return err
}
