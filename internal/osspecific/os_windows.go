package osspecific

import (
	"os"
)

func QuitSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

func SocketOptions(mode os.FileMode) interface{} {
	return nil
}
