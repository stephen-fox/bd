package passfd

import (
	"fmt"
	"io/fs"
	"net"
	"os"
)

// ShareableUnixListener calls ShareableListener with the network argument
// set to "unix" and the addr argument set to the specified file path.
//
// This function attempts to first remove the file at the specified
// path before starting the new listener. It then chmods the file
// with the specified permission bits.
func ShareableUnixListener(filePath string, perm fs.FileMode) (net.Listener, *os.File, error) {
	_ = os.Remove(filePath)

	listener, file, err := ShareableListener("unix", filePath)
	if err != nil {
		return nil, nil, err
	}

	err = os.Chmod(filePath, perm)
	if err != nil {
		_ = listener.Close()
		_ = file.Close()

		return nil, nil, fmt.Errorf("failed to chmod listener file - %w", err)
	}

	return listener, file, nil
}

// ShareableListener instantiaes a net.Listener using the net.Listen function
// and returns both it and its underlying file descriptor. The returned
// *os.File can be passed to child processes using the exec.Cmd.ExtraFiles
// field, or using a Unix socket.
//
// Once passed to a child process, the *os.File should be closed by the
// parent process. Closing the net.Listener will close it for both the
// parent and child processes - thus, it must be left open by the parent
// process until the child process no longer needs it.
func ShareableListener(network string, addr string) (net.Listener, *os.File, error) {
	listener, err := net.Listen(network, addr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to listen - %w", err)
	}

	fileListener, ok := listener.(FdProvider)
	if !ok {
		_ = listener.Close()

		return nil, nil, fmt.Errorf("listener for net %q addr %q does not implement file method (data type is %T)", network, addr, listener)
	}

	file, err := fileListener.File()
	if err != nil {
		_ = listener.Close()

		return nil, nil, fmt.Errorf("failed to get listener's file descriptor - %w", err)
	}

	return listener, file, nil
}
