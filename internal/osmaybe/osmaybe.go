package osmaybe

import (
	"log"
	"os"
	"os/exec"
)

type MaybeDo struct {
	DryRunMode bool
	Logger     *log.Logger
}

func (o *MaybeDo) Chmod(name string, mode os.FileMode) error {
	if o.DryRunMode {
		o.Logger.Printf("chmod: %q - %v", name, mode)

		return nil
	} else {
		return os.Chmod(name, mode)
	}
}

func (o *MaybeDo) MkdirAll(path string, perm os.FileMode) error {
	if o.DryRunMode {
		o.Logger.Printf("mkdirall: %q - %v", path, perm)

		return nil
	} else {
		return os.MkdirAll(path, perm)
	}
}

func (o *MaybeDo) Mkdir(name string, perm os.FileMode) error {
	if o.DryRunMode {
		o.Logger.Printf("mkdir: %q - %v", name, perm)

		return nil
	} else {
		return os.Mkdir(name, perm)
	}
}

func (o *MaybeDo) WriteFileHumanReadable(name string, data []byte, perm os.FileMode) error {
	if o.DryRunMode {
		o.Logger.Printf("writefile: %q - %v - contents:\n'%s'", name, perm, data)

		return nil
	} else {
		return os.WriteFile(name, data, perm)
	}
}

func (o *MaybeDo) ExecCombinedOutput(exe *exec.Cmd) ([]byte, error) {
	if o.DryRunMode {
		o.Logger.Printf("execute: %q - (%q)", exe.String(), exe.Args)

		return nil, nil
	} else {
		return exe.CombinedOutput()
	}
}

func (o *MaybeDo) Remove(name string) error {
	if o.DryRunMode {
		o.Logger.Printf("remove: %q", name)

		return nil
	} else {
		return os.Remove(name)
	}
}
