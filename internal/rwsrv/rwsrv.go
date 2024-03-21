package rwsrv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const (
	NoClientFlag uint8 = iota
	BufferedOutputClientFlag
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

	// Dst is the io.Writer to copy clients writes to.
	//
	// It is automatically closed when the Server exits.
	Dst io.WriteCloser

	// OptLogFile is an optional log to write console output to.
	OptLogFile io.Writer
}

// New instantiates a Server and starts it.
func New(ctx context.Context, config Config) *Server {
	server := &Server{
		config:    config,
		toClients: make(chan writeEvent),
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
	toClients chan writeEvent
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
	currentConns := make(map[net.Conn]struct{})

	defer func() {
		o.config.Dst.Close()
		o.config.Src.Close()

		if o.err == nil {
			o.err = errors.New("unknown error")
		}

		errMsg := []byte(o.err.Error() + "\n")

		for conn := range currentConns {
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))

			_, _ = conn.Write(errMsg)

			_ = conn.Close()

			delete(currentConns, conn)
		}

		close(o.done)
	}()

	go func() {
		_, err := io.Copy(&writer{server: o}, o.config.Src)
		o.readDone <- err
	}()

	closeConns := make(chan net.Conn)

	fromProxBufMaxBytes := 1024
	fromProcBuf := bytes.NewBuffer(nil)

loop:
	select {
	case <-ctx.Done():
		o.err = ctx.Err()
		return
	case <-o.close:
		o.err = errors.New("daemon has shutdown")
		return
	case <-o.config.Listener.Done():
		o.err = fmt.Errorf("listener is done - %w", o.config.Listener.Err())
		return
	case err := <-o.readDone:
		if err != nil {
			o.err = fmt.Errorf("failed to read from reader - %w", err)
		} else {
			o.err = errors.New("read exited unexpectedly without error")
		}
		return
	case conn := <-o.config.Listener.Conns():
		setDeadLineErr := conn.SetReadDeadline(time.Now().Add(time.Second))
		if setDeadLineErr == nil {
			options := make([]byte, 1)

			_, _ = conn.Read(options)

			if fromProcBuf.Len() > 0 && options[0]&BufferedOutputClientFlag != 0 {
				_, err := fromProcBuf.WriteTo(conn)
				if err != nil {
					_ = conn.Close()
					goto loop
				}
			}

			_ = conn.SetReadDeadline(time.Time{})
		}

		go func() {
			_, _ = io.Copy(o.config.Dst, conn)
			select {
			case closeConns <- conn:
			case <-ctx.Done():
				_ = conn.Close()
			}
		}()

		currentConns[conn] = struct{}{}
	case closeThis := <-closeConns:
		_ = closeThis.Close()
		delete(currentConns, closeThis)
	case write := <-o.toClients:
		var n int
		var err error
		if o.config.OptLogFile != nil {
			n, err = o.config.OptLogFile.Write(write.b)
		} else {
			n = len(write.b)
		}

		write.cb <- writeEventResult{
			n:   n,
			err: err,
		}

		// TODO: Make buffering configurable.
		// TODO: Always do buffering if it is enabled.
		if len(currentConns) == 0 {
			fromProcBuf.Write(write.b)

			if fromProcBuf.Len() > fromProxBufMaxBytes {
				discard := fromProcBuf.Len() - fromProxBufMaxBytes

				io.CopyN(io.Discard, fromProcBuf, int64(discard))
			}
		}

		for conn := range currentConns {
			_, connErr := conn.Write(write.b)
			if connErr != nil {
				_ = conn.Close()
				delete(currentConns, conn)
			}
		}
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
	cb := make(chan writeEventResult, 1)

	select {
	case <-o.done:
		return 0, o.err
	case o.toClients <- writeEvent{
		b:  b,
		cb: cb,
	}:
	}

	select {
	case <-o.done:
		return 0, o.err
	case result := <-cb:
		return result.n, result.err
	}
}

// TODO: Refactor to use pointers and "ready" channel
// to indicate callback is complete.
type writeEvent struct {
	b  []byte
	cb chan<- writeEventResult
}

type writeEventResult struct {
	n   int
	err error
}
