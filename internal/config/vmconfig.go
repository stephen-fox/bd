package config

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gitlab.com/stephen-fox/bd/internal/ini"
	"gitlab.com/stephen-fox/bd/internal/nettools"
	"gitlab.com/stephen-fox/bd/internal/shellquote"
)

// VmConfig::General section and its parameters.
const (
	GeneralVmConfigSection = "General"

	AutostartParam     = "Autostart"
	SerialConsoleParam = "SerialConsole"
	InsertDiscParam    = "InsertDisc"
)

// VmConfig::PrestartDep section and its parameters.
const (
	PrestartDepVmConfigSecgtion = "PrestartDep"

	NameParam        = "Name"
	CreateExecParam  = "CreateExec"
	CleanupExecParam = "CleanupExec"
)

// VmConfig::BhyveArgs section and its parameters.
const (
	BhyveArgsVmsConfigSection = "BhyveArgs"
)

// Misc. global variables.
const (
	UefiVarsFileName = "uefi-vars.fd"
)

// ParseVmConfig parses a VmConfig from an io.Reader.
func ParseVmConfig(r io.Reader, vmName string) (*VmConfig, error) {
	config := &VmConfig{
		VmName: vmName,
	}

	err := ini.ParseSchema(r, config)
	if err != nil {
		return nil, err
	}

	return config, nil
}

// VmConfig represents a VM's configuration.
type VmConfig struct {
	VmName         string
	General        *VmGeneral
	VmPrestartDeps []VmPreStartDep
	BhyveArgs      *BhyveArgs
}

// Rules partly implements the ini.Schema interface.
func (o *VmConfig) Rules() ini.ParserRules {
	return ini.ParserRules{
		RequiredSections: []string{
			BhyveArgsVmsConfigSection,
		},
	}
}

// OnGlobalParam partly implements the ini.Schema interface.
func (o *VmConfig) OnGlobalParam(paramName string) (func(*ini.Param) error, ini.SchemaRule) {
	return nil, ini.SchemaRule{}
}

// OnSection partly implements the ini.Schema interface.
func (o *VmConfig) OnSection(name string, _ string) (func() (ini.SectionSchema, error), ini.SchemaRule) {
	switch name {
	case GeneralVmConfigSection:
		return func() (ini.SectionSchema, error) {
			o.General = &VmGeneral{}

			return o.General, nil
		}, ini.SchemaRule{Limit: 1}
	case PrestartDepVmConfigSecgtion:
		return func() (ini.SectionSchema, error) {
			o.VmPrestartDeps = append(o.VmPrestartDeps, VmPreStartDep{})

			return &o.VmPrestartDeps[len(o.VmPrestartDeps)-1], nil
		}, ini.SchemaRule{}
	case BhyveArgsVmsConfigSection:
		return func() (ini.SectionSchema, error) {
			o.BhyveArgs = &BhyveArgs{}

			return o.BhyveArgs, nil
		}, ini.SchemaRule{Limit: 1, ParamsAreList: true}
	default:
		return nil, ini.SchemaRule{}
	}
}

// Validate partly implements the ini.Schema interface.
func (o *VmConfig) Validate() error {
	return nil
}

func (o *VmConfig) CreatePrestartDeps(ctx context.Context, vars *Variables) error {
	var depsToCleanupOnError []VmPreStartDep

	for i, dep := range o.VmPrestartDeps {
		err := dep.Create(ctx, vars)
		if err != nil {
			var cleanupErrs []string

			cleanupCtx, cancelFn := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelFn()

			for _, cleanup := range depsToCleanupOnError {
				err := cleanup.Cleanup(cleanupCtx, vars)
				if err != nil {
					cleanupErrs = append(cleanupErrs, err.Error())
				}
			}

			var id string
			if dep.Name == "" {
				id = fmt.Sprintf("index: %d", i)
			} else {
				id = fmt.Sprintf("name %q (index: %d)", dep.Name, i)
			}

			var cleanupErrStr string
			if len(cleanupErrs) > 0 {
				cleanupErrStr = " - several cleanup errors also occurred:" +
					strings.Join(cleanupErrs, " ")
			}

			return fmt.Errorf("failed to create PrestartDep for %s - %w%s",
				id, err, cleanupErrStr)
		}

		if len(dep.CleanupExecs) > 0 {
			depsToCleanupOnError = append(depsToCleanupOnError, dep)
		}
	}

	return nil
}

func (o *VmConfig) CleanupPrestartDeps(ctx context.Context, vars *Variables, logger *log.Logger) {
	for i, dep := range o.VmPrestartDeps {
		err := dep.Cleanup(ctx, vars)
		if err != nil {
			var id string
			if dep.Name == "" {
				id = fmt.Sprintf("index: %d", i)
			} else {
				id = fmt.Sprintf("name %q (index: %d)", dep.Name, i)
			}

			logger.Printf("failed to cleanup PrestartDep %s - %s",
				id, err)
		}
	}
}

func (o *VmConfig) RenderBhyveArgs(vars *Variables, appConfig *AppConfig) ([]string, error) {
	var args []string

	var addedCdDrive bool
	var lastPciSlot uint64

	for _, arg := range o.BhyveArgs.Args {
		replaced, err := vars.ReplaceVars(arg)
		if err != nil {
			return nil, fmt.Errorf("failed to replace string(s) in arg %q - %w",
				arg, err)
		}

		if o.General.InsertDisc != "" && !addedCdDrive && len(args) > 0 && args[len(args)-1] == "-s" {
			replaced = strings.TrimSpace(replaced)

			pciSlotStr, _, foundSep := strings.Cut(replaced, ",")
			if !foundSep {
				return nil, fmt.Errorf("insert-disc mode: failed to find ',' delimiter in pci string when adding cd drive: %q",
					replaced)
			}

			currentPciSlot, err := strconv.ParseUint(pciSlotStr, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("insert-disc mode: failed to parse pci slot number when adding cd drive: %q in %q - %w",
					pciSlotStr, replaced, err)
			}

			possibleSlot := currentPciSlot - 1

			// 0 - skip
			// 1 - 1-1 > 0 = skip
			// 3 - 3-1 > 1 = add
			if currentPciSlot > 0 && possibleSlot > lastPciSlot {
				discPathRepalced, err := vars.ReplaceVars(o.General.InsertDisc)
				if err != nil {
					return nil, fmt.Errorf("insert-disc mode: failed to replace variables in insert disc param: %w", err)
				}

				if !filepath.IsAbs(discPathRepalced) {
					discPathRepalced = filepath.Join(appConfig.General.BootMediaDir, discPathRepalced)
				}

				args = append(args, fmt.Sprintf("%d,ahci-cd,%s",
					possibleSlot, discPathRepalced))

				args = append(args, "-s")

				addedCdDrive = true
			} else {
				lastPciSlot = currentPciSlot
			}
		}

		args = append(args, replaced)
	}

	if o.General.InsertDisc != "" && !addedCdDrive {
		return nil, fmt.Errorf("insert-disc mode: failed to add cd drive to bhyve args: %q", args)
	}

	args = append(args, o.VmName)

	return args, nil
}

type VmGeneral struct {
	Autostart bool

	// Enable serial console access using the 'console' mode
	// (requires that bhyve use the serial console 'stdio' mode - e.g.,
	// '-s 31,lpc -l com1,stdio'
	SerialConsole bool

	InsertDisc string
}

// RequiredParams partly implements the ini.SectionSchema interface.
func (o *VmGeneral) RequiredParams() []string {
	return nil
}

// OnParam partly implements the ini.SectionSchema interface.
func (o *VmGeneral) OnParam(paramName string) (func(*ini.Param) error, ini.SchemaRule) {
	switch paramName {
	case AutostartParam:
		return func(p *ini.Param) error {
			enabled, err := strconv.ParseBool(p.Value)
			if err != nil {
				return err
			}

			o.Autostart = enabled

			return nil
		}, ini.SchemaRule{Limit: 1}
	case SerialConsoleParam:
		return func(p *ini.Param) error {
			enabled, err := strconv.ParseBool(p.Value)
			if err != nil {
				return err
			}

			o.SerialConsole = enabled

			return nil
		}, ini.SchemaRule{Limit: 1}
	case InsertDiscParam:
		return func(p *ini.Param) error {
			o.InsertDisc = p.Value

			return nil
		}, ini.SchemaRule{Limit: 1}
	default:
		return nil, ini.SchemaRule{}
	}
}

// Validate partly implements the ini.SectionSchema interface.
func (o *VmGeneral) Validate() error {
	return nil
}

type VmPreStartDep struct {
	Name         string
	CreateExecs  [][]string
	CleanupExecs [][]string
}

// RequiredParams partly implements the ini.SectionSchema interface.
func (o *VmPreStartDep) RequiredParams() []string {
	return []string{CreateExecParam}
}

// OnParam partly implements the ini.SectionSchema interface.
func (o *VmPreStartDep) OnParam(paramName string) (func(*ini.Param) error, ini.SchemaRule) {
	switch paramName {
	case NameParam:
		return func(p *ini.Param) error {
			o.Name = p.Value

			return nil
		}, ini.SchemaRule{Limit: 1}
	case CreateExecParam:
		return func(p *ini.Param) error {
			argv, err := shellquote.Split(p.Value)
			if err != nil {
				return fmt.Errorf("failed to split create exec argv line: %q - %w", p.Value, err)
			}

			o.CreateExecs = append(o.CreateExecs, argv)

			return nil
		}, ini.SchemaRule{}
	case CleanupExecParam:
		return func(p *ini.Param) error {
			argv, err := shellquote.Split(p.Value)
			if err != nil {
				return fmt.Errorf("failed to split cleanup exec argv line: %q - %w", p.Value, err)
			}

			o.CleanupExecs = append(o.CleanupExecs, argv)

			return nil
		}, ini.SchemaRule{}
	default:
		return nil, ini.SchemaRule{}
	}
}

// Validate partly implements the ini.SectionSchema interface.
func (o *VmPreStartDep) Validate() error {
	return nil
}

func (o *VmPreStartDep) Create(ctx context.Context, vars *Variables) error {
	if o.Name != "" {
		err := isStringSafeForVariableName(o.Name)
		if err != nil {
			return fmt.Errorf("name is not safe for variable use: %q - %w",
				o.Name, err)
		}
	}

	for i, argvOrig := range o.CreateExecs {
		argv := make([]string, len(argvOrig))
		copy(argv, argvOrig)

		for i, arg := range argv {
			replaced, err := vars.ReplaceVars(arg)
			if err != nil {
				return fmt.Errorf("failed to replace string(s) in argument %q of %q - %w",
					arg, strings.Join(argv, " "), err)
			}

			argv[i] = replaced
		}

		app := exec.CommandContext(ctx, argv[0], argv[1:]...)

		var exitErr *exec.ExitError

		stdout, err := app.Output()
		switch {
		case err == nil:
			// Keep going.
		case errors.As(err, &exitErr):
			return fmt.Errorf("failed to exec %q - %w - stderr: %q | stdout: %q",
				app.String(), err, exitErr.Stderr, stdout)
		default:
			return fmt.Errorf("failed to exec %q - %w",
				app.String(), err)
		}

		if i == len(o.CreateExecs)-1 && o.Name != "" {
			err := vars.AddVariable(PrestartDepVariableType, prestartDepConfigVarsPrefix+"_"+o.Name, strings.TrimSpace(string(stdout)))
			if err != nil {
				return fmt.Errorf("failed to add variable for %q - %q",
					o.Name, err)
			}
		}
	}

	return nil
}

func (o *VmPreStartDep) Cleanup(ctx context.Context, vars *Variables) error {
	for _, argvOrig := range o.CleanupExecs {
		argv := make([]string, len(argvOrig))
		copy(argv, argvOrig)

		for i, arg := range argv {
			replaced, err := vars.ReplaceVars(arg)
			if err != nil {
				return fmt.Errorf("failed to replace string(s) in argument %q of %q - %w",
					arg, strings.Join(argv, " "), err)
			}

			argv[i] = replaced
		}

		app := exec.CommandContext(ctx, argv[0], argv[1:]...)

		output, err := app.CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to exec %q - %w | stdout+err: %q",
				app.String(), err, output)
		}
	}

	return nil
}

type BhyveArgs struct {
	Args []string
}

// RequiredParams partly implements the ini.SectionSchema interface.
func (o *BhyveArgs) RequiredParams() []string {
	return nil
}

// OnParam partly implements the ini.SectionSchema interface.
func (o *BhyveArgs) OnParam(paramName string) (func(*ini.Param) error, ini.SchemaRule) {
	return func(p *ini.Param) error {
		words, err := shellquote.Split(p.Name)
		if err != nil {
			return fmt.Errorf("failed to split bhyve argument line: %q - %w",
				p.Name, err)
		}

		o.Args = append(o.Args, words...)

		return nil
	}, ini.SchemaRule{}
}

// Validate partly implements the ini.SectionSchema interface.
func (o *BhyveArgs) Validate() error {
	if len(o.Args) == 0 {
		return fmt.Errorf("please specify at least one bhyve argument")
	}

	return nil
}

func NewVmConfigFile(w io.Writer) error {
	mac, err := nettools.GenMacAddr()
	if err != nil {
		return fmt.Errorf("failed to generate a mac address - %w", err)
	}

	const ifaceVariableStr = "${" + prestartDepConfigVarsPrefix + "_tap}"

	// Using a separate variable for the config contents so we can preview
	// it in the editor.
	contents := `[` + GeneralVmConfigSection + `]
` + AutostartParam + ` = false
` + SerialConsoleParam + ` = true
` + InsertDiscParam + ` = example.iso

[` + PrestartDepVmConfigSecgtion + `]
# The following "` + NameParam + ` makes the stdout of the last "` + CreateExecParam + `
# accessible using a magic variable named "` + ifaceVariableStr + `":
` + NameParam + ` = tap
` + CreateExecParam + ` = ifconfig tap create description bhyve/${` + vmNameVarName + `}
` + CleanupExecParam + ` = ifconfig ` + ifaceVariableStr + ` destroy

# [` + PrestartDepVmConfigSecgtion + `]
# ` + CreateExecParam + ` = ifconfig some_bridge addm ` + ifaceVariableStr + ` private ` + ifaceVariableStr + `

[` + BhyveArgsVmsConfigSection + `]
-A
-D
-H
-P
-c 2
-m 4G
# Uncomment the line below for OSes like OpenBSD that use
# unimplemented Model Specific Registers.
# -w
-s 0,hostbridge
-s 3,virtio-blk,${` + vmStorageDirVarName + `}/disk0.img
-s 10,virtio-net,` + ifaceVariableStr + `,mac=` + mac + `
-s 20,virtio-rnd
-s 31,lpc
-l com1,stdio
-l bootrom,/usr/local/share/uefi-firmware/BHYVE_UEFI.fd,${` + vmStorageDirVarName + `}/` + UefiVarsFileName + `
`

	_, err = w.Write([]byte(contents))

	return err
}

var shellVarNameRegex = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func isStringSafeForVariableName(str string) error {
	if str == "" {
		return fmt.Errorf("name is empty")
	}

	isAllowed := shellVarNameRegex.MatchString(str)
	if isAllowed {
		return nil
	}

	return fmt.Errorf("contains invalid characters, must match regex: %q",
		shellVarNameRegex.String())
}
