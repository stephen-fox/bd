package passfd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
)

// ReadCloserUpdater represents an io.ReadCloser that can be updated.
type ReadCloserUpdater interface {
	Set(io.ReadCloser) error
}

// ReadCloserUpdaterToRecvFn creates an on recv function for use with Client
// for the given ReadCloserUpdater.
func ReadCloserUpdaterToRecvFn(r ReadCloserUpdater) func(*os.File) error {
	return func(f *os.File) error {
		return r.Set(f)
	}
}

// WriteCloserUpdater represents an io.WriteCloser that can be updated.
type WriteCloserUpdater interface {
	Set(io.WriteCloser) error
}

// WriteCloserUpdaterToRecvFn creates an on recv function for use with Client
// for the given WriteCloserUpdater.
func WriteCloserUpdaterToRecvFn(w WriteCloserUpdater) func(*os.File) error {
	return func(f *os.File) error {
		return w.Set(f)
	}
}

// NewClient instantiates a Client.
func NewClient(ctx context.Context, unixConn *net.UnixConn, onRecvFns []func(*os.File) error) *Client {
	client := &Client{
		unixConn: unixConn,
		recvFns:  onRecvFns,
		getErr:   make(chan error),
		done:     make(chan struct{}),
	}

	go client.loop(ctx)

	return client
}

// Client executes the provided functions each time new file descriptors
// are received on the underlying Unix socket. The Client exits when
// the provided context.Context is marked as done or an error occurs.
//
// File descriptors are passed to each function based on their index
// in the array received by the Unix socket. For example, if the slice
// contains two functions, the Client will expect two file descriptors
// to be pushed to the Unix socket in a single message. fd[0] would
// then be passed to recvFns[0] and fd[1] would be passed to recvFns[1].
type Client struct {
	unixConn *net.UnixConn
	recvFns  []func(*os.File) error
	getErr   chan error
	done     chan struct{}
	err      error
}

// Done returns a channel that is closed when the parent context.Context
// is marked as done or when an error occurs.
func (o *Client) Done() <-chan struct{} {
	return o.done
}

// Err returns a non-nil error explaining why the Client exited.
// This method should only be called after the channel returned
// by Done is closed.
func (o *Client) Err() error {
	return o.err
}

func (o *Client) loop(ctx context.Context) {
	defer func() {
		if o.err == nil {
			o.err = errors.New("exited due to unknown error")
		}

		o.unixConn.Close()

		close(o.done)
	}()

	go o.getFdsLoop()

	for {
		select {
		case <-ctx.Done():
			o.err = ctx.Err()
			return
		case err := <-o.getErr:
			o.err = fmt.Errorf("failed to get fds - %w", err)
			return
		}
	}
}

func (o *Client) getFdsLoop() {
	err := o.getFdsLoopWithError()
	if err == nil {
		err = errors.New("get fds loop exited unexpectedly without error")
	}

	select {
	case <-o.done:
	case o.getErr <- err:
	}
}

func (o *Client) getFdsLoopWithError() error {
	for {
		expNumFds := len(o.recvFns)
		if expNumFds == 0 {
			return errors.New("on recv fns slice is empty")
		}

		fds, err := Get(o.unixConn, expNumFds, nil)
		if err != nil {
			return fmt.Errorf("fd get failed - %w", err)
		}

		for i, fd := range fds {
			err = o.recvFns[i](fd)
			if err != nil {
				return fmt.Errorf("recv fn for fd index %d failed - %w", i, err)
			}
		}
	}
}
