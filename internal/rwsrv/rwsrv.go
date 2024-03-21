package rwsrv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
)

// ListenerCtx implements useful context.Context methods and allows
// callers to learn of new connections as they are accepted.
type ListenerCtx interface {
	Conns() <-chan net.Conn
	Done() <-chan struct{}
	Err() error
}

// Config configures a Server.
type Config struct {
	// Listener is the ListenerCtx to listen for connections on.
	Listener ListenerCtx

	// Src is the io.ReadCloser to copy to clients.
	//
	// It is automatically closed when the Server exits.
	Src io.ReadCloser

	// Dst is the io.Writer to copy clients' writes to.
	//
	// It is automatically closed when the Server exits.
	Dst io.WriteCloser

	// OptBuf is the number of bytes to save of Src reads.
	// If greater than zero, reads will be buffered such
	// that newly connected clients will receive the
	// buffered data when they first connect.
	OptBufSz uint

	// OptSrcLog is an optional log to copy Src reads to.
	OptSrcLog io.Writer
}

// New instantiates a Server and starts it.
func New(ctx context.Context, config Config) *Server {
	server := &Server{
		config:    config,
		toClients: make(chan *writeEvent),
		readDone:  make(chan error, 1),
		close:     make(chan struct{}),
		done:      make(chan struct{}),
	}

	go server.loop(ctx)

	return server
}

// Server serves an io.Reader and an io.Writer to clients.
//
// Clients' writes are sent to the io.Writer and any data read from the
// io.Reader are written to the clients.
//
// Optionally, reads from the io.Reader can be buffered so that clients
// receive the buffered data when they first connect.
type Server struct {
	config    Config
	toClients chan *writeEvent
	readDone  chan error
	close     chan struct{}
	done      chan struct{}
	err       error
}

// Done returns a channel that is closed when the Server exits.
func (o *Server) Done() <-chan struct{} {
	return o.done
}

// Err returns a non-nil error explaining why the server exited.
// This method should only be called after the channel returned
// by Done is closed.
func (o *Server) Err() error {
	return o.err
}

// Close stops the Server.
func (o *Server) Close() error {
	select {
	case <-o.done:
		return o.err
	case o.close <- struct{}{}:
		return nil
	}
}

func (o *Server) loop(ctx context.Context) {
	// Use our own Context so that non-context errors
	// trigger a Context cancelation.
	var cancelFn func()
	ctx, cancelFn = context.WithCancel(ctx)
	defer cancelFn()

	o.err = o.loopWithError(ctx)

	o.config.Dst.Close()
	o.config.Src.Close()

	if o.err == nil {
		o.err = errors.New("unknown error")
	}

	close(o.done)
}

func (o *Server) loopWithError(ctx context.Context) error {
	currentConns := make(map[net.Conn]struct{})
	defer func() {
		for conn := range currentConns {
			_ = conn.Close()

			delete(currentConns, conn)
		}
	}()

	go func() {
		_, err := io.Copy(&writer{server: o}, o.config.Src)
		if err == nil {
			err = errors.New("reader exited unexpectedly with nil error")
		}

		o.readDone <- err
	}()

	closeConns := make(chan net.Conn)

	var readBuf *bytes.Buffer
	if o.config.OptBufSz > 0 {
		readBuf = bytes.NewBuffer(nil)
	}

loop:
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.close:
		return errors.New("server was closed")
	case <-o.config.Listener.Done():
		return fmt.Errorf("listener is done - %w", o.config.Listener.Err())
	case err := <-o.readDone:
		return fmt.Errorf("failed to read from reader - %w", err)
	case conn := <-o.config.Listener.Conns():
		if readBuf != nil && readBuf.Len() > 0 {
			_, err := conn.Write(readBuf.Bytes())
			if err != nil {
				_ = conn.Close()
				goto loop
			}
		}

		go func() {
			_, _ = io.Copy(o.config.Dst, conn)
			select {
			case closeConns <- conn:
			case <-ctx.Done():
			}
		}()

		currentConns[conn] = struct{}{}
	case closeThis := <-closeConns:
		_ = closeThis.Close()
		delete(currentConns, closeThis)
	case write := <-o.toClients:
		if o.config.OptSrcLog != nil {
			write.n, write.err = o.config.OptSrcLog.Write(write.b)
		} else {
			write.n = len(write.b)
		}

		if readBuf != nil {
			readBuf.Write(write.b)

			if readBuf.Len() > int(o.config.OptBufSz) {
				discard := readBuf.Len() - int(o.config.OptBufSz)

				io.CopyN(io.Discard, readBuf, int64(discard))
			}
		}

		for conn := range currentConns {
			_, connErr := conn.Write(write.b)
			if connErr != nil {
				_ = conn.Close()
				delete(currentConns, conn)
			}
		}

		// It is very important that we unblock the
		// write here because the []byte is shared
		// between the reader Go routine and this
		// one. If we unblock too early, the []byte
		// may get written too while we are copying
		// it into the conn. Such a case leads to
		// malformed data being written to the client.
		close(write.done)
	}

	goto loop
}

type writer struct {
	server *Server
}

func (o *writer) Write(b []byte) (int, error) {
	return o.server.write(b)
}

func (o *Server) write(b []byte) (int, error) {
	write := &writeEvent{
		b:    b,
		done: make(chan struct{}),
	}

	select {
	case <-o.done:
		return 0, o.err
	case o.toClients <- write:
	}

	select {
	case <-o.done:
		return 0, o.err
	case <-write.done:
		return write.n, write.err
	}
}

type writeEvent struct {
	b    []byte
	done chan struct{}
	n    int
	err  error
}
