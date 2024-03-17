package hsio

import (
	"errors"
	"io"
	"sync"
)

// NewWriteCloser instantiates a WriteCloser.
func NewWriteCloser() *WriteCloser {
	return &WriteCloser{}
}

// WriteCloser is a hot-swappable implementation of io.WriteCloser.
type WriteCloser struct {
	rwMu sync.RWMutex
	w    io.WriteCloser
	err  error
}

// Set swaps out the existing io.WriteCloser for w.
func (o *WriteCloser) Set(w io.WriteCloser) error {
	o.rwMu.Lock()
	defer o.rwMu.Unlock()

	if o.err != nil {
		return o.err
	}

	o.w = w

	return nil
}

// Close closes the WriteCloser.
func (o *WriteCloser) Close() error {
	o.rwMu.Lock()
	defer o.rwMu.Unlock()

	if o.err != nil {
		return o.err
	}

	o.err = errors.New("writer closed")

	if o.w != nil {
		o.w.Close()
		o.w = nil
	}

	return nil
}

func (o *WriteCloser) Write(b []byte) (int, error) {
	o.rwMu.RLock()
	defer o.rwMu.RUnlock()

	if o.err != nil {
		return 0, o.err
	}

	// TODO: Maybe we should cache the data instead?
	if o.w == nil {
		return len(b), nil
	}

	_, _ = o.w.Write(b)

	return len(b), nil
}
