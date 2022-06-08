package osspecific

import (
	"errors"
	"os"
	"syscall"
)

func QuitSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

func SocketOptions(mode os.FileMode) interface{} {
	return nil
}

func SysProcAttrForChildProc(username string) (*syscall.SysProcAttr, error) {
	return nil, errors.New("sys proc attr for user not supported on windows")
}
