package passfd

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// SharableUnixSocketpair creates two Unix sockets using socketpair(2)
// and returns them in an exec.Cmd-friendly format.
//
// The first return value should be used by the caller to communicate with
// the second socket. The second socket can be passed to a child process
// using the exec.Cmd.ExtraFiles field.
//
// Typically, callers will set socketType to syscall.SOCK_STREAM
// and protocol to 0.
func SharableUnixSocketpair(socketType int, protocol int) (*net.UnixConn, *os.File, error) {
	files, err := SocketpairFiles(syscall.AF_LOCAL, socketType, protocol)
	if err != nil {
		return nil, nil, fmt.Errorf("socketpair files failed - %w", err)
	}
	defer files[0].Close()

	unixConn, err := UnixConnFromFile(files[0])
	if err != nil {
		_ = files[1].Close()

		return nil, nil, fmt.Errorf("failed to create unix conn from file - %w", err)
	}

	return unixConn, files[1], nil
}

// SocketpairFiles calls socketpair(2) and returns two *os.File representing
// the sockets. For more information, refer to man 2 socketpair.
func SocketpairFiles(domain int, sockType int, proto int) ([2]*os.File, error) {
	var files [2]*os.File

	fds, err := syscall.Socketpair(domain, sockType, proto)
	if err != nil {
		return files, fmt.Errorf("socketpair syscall failed - %w", err)
	}

	f0 := os.NewFile(uintptr(fds[0]), "")
	if f0 == nil {
		_ = syscall.Close(fds[0])
		_ = syscall.Close(fds[1])

		return files, fmt.Errorf("os new file returned a nil file for first fd (%d) - fd is invalid", fds[0])
	}

	f1 := os.NewFile(uintptr(fds[1]), "")
	if f1 == nil {
		_ = f0.Close()
		_ = syscall.Close(fds[1])

		return files, fmt.Errorf("os new file returned a nil file for second fd (%d) - fd is invalid", fds[1])
	}

	files[0] = f0
	files[1] = f1

	return files, nil
}

// UnixConnFromFd converts the provided file descriptor to a *net.UnixConn.
// An optional name can be provided if desired.
func UnixConnFromFd(fd uintptr, optName string) (*net.UnixConn, error) {
	file := os.NewFile(fd, optName)
	if file == nil {
		return nil, fmt.Errorf("os new file returned a nil file for fd %d - fd is invalid", fd)
	}
	defer file.Close()

	unixConn, err := UnixConnFromFile(file)
	if err != nil {
		return nil, fmt.Errorf("unixconnfromfile failed - %w", err)
	}

	return unixConn, nil
}

// UnixConnFromFile converts an *os.File to a *net.UnixConn.
//
// The caller should close the provided *os.File after
// calling this function.
func UnixConnFromFile(file *os.File) (*net.UnixConn, error) {
	conn, err := net.FileConn(file)
	if err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("net file conn failed - %w", err)
	}

	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		_ = conn.Close()
		_ = file.Close()

		return nil, fmt.Errorf("expected conn to be *net.UnixConn - got %T", conn)
	}

	return unixConn, nil
}
