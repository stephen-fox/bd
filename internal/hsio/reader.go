package hsio

import (
	"errors"
	"io"
	"log"
)

// NewReadCloser instantiates a ReadCloser.
func NewReadCloser() *ReadCloser {
	r, w := io.Pipe()

	reader := &ReadCloser{
		r:    r,
		w:    w,
		set:  make(chan io.ReadCloser),
		wake: make(chan io.Reader),
		read: make(chan readCallback),
		clos: make(chan struct{}),
		done: make(chan struct{}),
	}

	go reader.loop()

	return reader
}

// ReadCloser is a hot-swappable implementation of the io.ReadCloser interface.
type ReadCloser struct {
	r    *io.PipeReader
	w    *io.PipeWriter
	set  chan io.ReadCloser
	wake chan io.Reader
	read chan readCallback
	clos chan struct{}
	done chan struct{}
	err  error
}

// Set swaps out the existing io.ReadCloser for r.
func (o *ReadCloser) Set(r io.ReadCloser) error {
	select {
	case <-o.done:
		return o.err
	case o.set <- r:
		return nil
	}
}

// Close closes the ReadCloser.
func (o *ReadCloser) Close() error {
	select {
	case <-o.done:
		return o.err
	case o.clos <- struct{}{}:
		return nil
	}
}

func (o *ReadCloser) Read(b []byte) (int, error) {
	return o.r.Read(b)
}

func (o *ReadCloser) readOld(b []byte) (int, error) {
retry:
	cb := readCallback{
		b:     b,
		ready: make(chan struct{}),
	}

	select {
	case <-o.done:
		return 0, o.err
	case o.read <- cb:
		log.Printf("TODO: read []byte sent")
		// Keep going.
	}

	select {
	case <-o.done:
		return 0, o.err
	case <-cb.ready:
		if cb.err != nil {
			log.Printf("TODO: retry read - %v", cb.err)
			goto retry
		}

		log.Printf("TODO: read %d", cb.n)

		return cb.n, nil
	}
}

type readCallback struct {
	b     []byte
	ready chan struct{}
	n     int
	err   error
}

func (o *ReadCloser) loop() {
	var current io.ReadCloser

	defer func() {
		if o.err == nil {
			o.err = errors.New("exited due to unknown error")
		}

		if current != nil {
			current.Close()
		}

		o.r.Close()
		o.w.Close()

		close(o.done)
	}()

	// go o.readFrom()

	for {
		select {
		case <-o.clos:
			o.err = errors.New("reader closed")
			return
		case r := <-o.set:
			if current != nil {
				current.Close()
			}

			current = r

			go io.Copy(o.w, r)

			// go func() {
			// 	select {
			// 	case <-o.done:
			// 	case o.wake <- r:
			// 	}
			// }()
		}
	}
}

func (o *ReadCloser) readFrom() {
	var cachedRead *readCallback
	var r io.Reader

	for {
		select {
		case <-o.done:
			return
		case cb := <-o.read:
			cachedRead = &cb
		case r = <-o.wake:
			// New reader available.
		}

		if r != nil && cachedRead != nil {
			log.Printf("TODO: do cached read")

			cachedRead.n, cachedRead.err = r.Read(cachedRead.b)

			close(cachedRead.ready)

			cachedRead = nil
		}
	}
}
