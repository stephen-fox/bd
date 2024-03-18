package passfd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
)

type ReadCloserUpdater interface {
	Set(io.ReadCloser) error
}

type WriteCloserUpdater interface {
	Set(io.WriteCloser) error
}

// NewFdUpdaterFnBuilder instantiates a FdUpdaterFnBuilder.
func NewFdUpdaterFnBuilder() *FdUpdaterFnBuilder {
	return &FdUpdaterFnBuilder{}
}

// FdUpdaterFnBuilder assists in generating the map of file descriptor index
// number to set functions used by Client.
type FdUpdaterFnBuilder struct {
	idxToFns map[int]func(*os.File) error
}

// AddReader adds the specified ReadCloser to the map. Its index will
// be equal to the length of the map prior to adding it. In other
// words, if the first call to the builder is AddReader, then the
// ReadCloser will be mapped to index 0.
func (o *FdUpdaterFnBuilder) AddReader(r ReadCloserUpdater) *FdUpdaterFnBuilder {
	i := o.init()

	o.idxToFns[i] = func(fd *os.File) error {
		return r.Set(fd)
	}

	return o
}

// AddWriter adds the specified WriteCloser to the map. Its index will
// be equal to the length of the map prior to adding it. In other
// words, if the first call to the builder is AddWriter, then the
// WriteCloser will be mapped to index 0.
func (o *FdUpdaterFnBuilder) AddWriter(w WriteCloserUpdater) *FdUpdaterFnBuilder {
	i := o.init()

	o.idxToFns[i] = func(fd *os.File) error {
		return w.Set(fd)
	}

	return o
}

func (o *FdUpdaterFnBuilder) init() int {
	if o.idxToFns == nil {
		o.idxToFns = make(map[int]func(*os.File) error)
	}

	return len(o.idxToFns)
}

// Build returns the current map of file descriptor index number to
// set function.
func (o *FdUpdaterFnBuilder) Build() map[int]func(*os.File) error {
	return o.idxToFns
}

// NewClient instantiates a Client.
func NewClient(ctx context.Context, unixConn FdConn, idxToFns map[int]func(*os.File) error) *Client {
	client := &Client{
		unixConn: unixConn,
		setFns:   idxToFns,
		getErr:   make(chan error),
		done:     make(chan struct{}),
	}

	go client.loop(ctx)

	return client
}

// Client executes the provided os.File set function each time new file
// descriptors are pushed to underlying UnixConn. The Client exits when
// the provided context.Context is marked as done.
//
// Each function is mapped to the slice of os.File pushed to the UnixConn
// using a map type. In other words, if the map consists of two elements,
// the Client will expect two file descriptors to be pushed to the UnixConn.
// The Clientr will then use the index number from the map to select the
// appropriate set function.
//
// The FdUpdaterFnBuilder can be used to simplify the generation of
// this map.
type Client struct {
	unixConn FdConn
	setFns   map[int]func(*os.File) error
	getErr   chan error
	done     chan struct{}
	err      error
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
		expNumFds := len(o.setFns)
		if expNumFds == 0 {
			return errors.New("indexes to set fns map is empty")
		}

		fds, err := Get(o.unixConn, expNumFds, nil)
		if err != nil {
			return fmt.Errorf("fd get failed - %w", err)
		}

		log.Printf("TODO: got fds %v - setting them...", fds)

		for i, fd := range fds {
			fn, ok := o.setFns[i]
			if !ok {
				return fmt.Errorf("indexes to set fns map is missing index %d", i)
			}

			err = fn(fd)
			if err != nil {
				return fmt.Errorf("set fn for fd index %d failed - %w", i, err)
			}
		}
	}
}
