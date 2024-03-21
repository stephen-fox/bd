package lctx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
)

// ListenUnixPath creates a Unix socket at the provided file path and
// returns a new ListenerCtx containing the socket's net.Listener.
func ListenUnixPath(ctx context.Context, filePath string, perm os.FileMode) (*ListenerCtx, error) {
	_ = os.Remove(filePath)

	listener, err := net.Listen("unix", filePath)
	if err != nil {
		return nil, err
	}

	err = os.Chmod(filePath, perm)
	if err != nil {
		_ = listener.Close()
		_ = os.Remove(filePath)

		return nil, fmt.Errorf("failed to chmod unix socket - %w", err)
	}

	return FromNetListener(ctx, listener), nil
}

// FromFileDescriptor converts the provided file descriptor to a net.Listener
// and wraps it with a ListenerCtx.
func FromFileDescriptor(ctx context.Context, fd uintptr, optName string) (*ListenerCtx, error) {
	file := os.NewFile(fd, optName)
	if file == nil {
		return nil, fmt.Errorf("os new file returned a nil file for fd %d - invalid file descriptor", fd)
	}
	defer file.Close()

	listener, err := net.FileListener(file)
	if err != nil {
		return nil, fmt.Errorf("net file listener failed - %w", err)
	}

	return FromNetListener(ctx, listener), nil
}

// FromNetListener returns a new ListenerCtx that wraps the
// provided net.Listener.
func FromNetListener(ctx context.Context, listener net.Listener) *ListenerCtx {
	l := &ListenerCtx{
		listener: listener,
		conns:    make(chan net.Conn),
		done:     make(chan struct{}),
	}

	go l.loop(ctx)

	return l
}

// ListenerCtx wraps a net.Listener, providing a more async-friendly
// API by implementing some of the context.Context interface's methods
// and providing newly-accepted net.Conn objects using a channel.
//
// Both it and the inner net.Listener can be shutdown by calling
// the Close method or by canceling the parent context.Context.
type ListenerCtx struct {
	listener net.Listener
	conns    chan net.Conn
	done     chan struct{}
	err      error
}

// Close closes the inner net.Listener.
func (o *ListenerCtx) Close() error {
	return o.listener.Close()
}

// Conns returns a channel that receives newly-accepted net.Conn objects
// provided by the inner net.Listener.
func (o *ListenerCtx) Conns() <-chan net.Conn {
	return o.conns
}

// Done returns a channel that is closed when the inner net.Listener is closed
// or the parent context.Context is marked as done.
func (o *ListenerCtx) Done() <-chan struct{} {
	return o.done
}

// Err returns a non-nil error explaining why the ListenerCtx exited.
// This method should only be called after the channel returned by Done
// is closed.
func (o *ListenerCtx) Err() error {
	return o.err
}

func (o *ListenerCtx) loop(ctx context.Context) {
	defer func() {
		if o.err != nil {
			o.err = errors.New("exited due to unknown error")
		}

		_ = o.listener.Close()

		close(o.done)
	}()

	for {
		conn, err := o.listener.Accept()

		select {
		case <-ctx.Done():
			o.err = ctx.Err()
			if conn != nil {
				_ = conn.Close()
			}
			return
		default:
			// Keep going.
		}

		if err != nil {
			o.err = err
			return
		}

		select {
		case <-ctx.Done():
			o.err = ctx.Err()
			_ = conn.Close()
			return
		case o.conns <- conn:
			// OK.
		}
	}
}
