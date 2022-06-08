//go:build !windows

package osspecific

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"syscall"

	"github.com/stephen-fox/goss"
)

func QuitSignals() []os.Signal {
	return []os.Signal{syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT}
}

func SocketOptions(mode os.FileMode) interface{} {
	return &goss.UnixListenerOptions{
		TryRemove: true,
		FileMode:  mode,
	}
}

func SysProcAttrForChildProc(username string) (*syscall.SysProcAttr, error) {
	account, err := user.Lookup(username)
	if err != nil {
		return nil, fmt.Errorf("failed to lookup user '%s' - %w", username, err)
	}

	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("failed to parse uid - %w", err)
	}

	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("failed to parse gid - %w", err)
	}

	return &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid:    uint32(uid),
			Gid:    uint32(gid),
			Groups: nil,
		},
	}, nil
}
