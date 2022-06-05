//go:build !windows

package osspecific

import (
	"os"
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
