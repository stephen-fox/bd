package passfd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
)

// ListenerCtx implements useful context.Context methods and allows
// callers to learn of new connections as they are accepted.
type ListenerCtx interface {
	Conns() <-chan net.Conn
	Done() <-chan struct{}
	Err() error
}

// ServeListener serves file descriptors on the provided listener.
func ServeListener(ctx context.Context, listener ListenerCtx) *ListenerServer {
	server := &ListenerServer{
		listener: listener,
		fds:      make(chan fdsReady),
		done:     make(chan struct{}),
	}

	go server.loop(ctx)

	return server
}

// ListenerServer serves file descriptors to any clients that connect to it.
type ListenerServer struct {
	listener ListenerCtx
	fds      chan fdsReady
	done     chan struct{}
	err      error
}

// Done returns a channel that is closed when the ListenerServer exits.
func (o *ListenerServer) Done() <-chan struct{} {
	return o.done
}

// Err returns a non-nil error explaning why the ListenerServer has
// exited. It should only be called once the channel returned by
// Done is closed.
func (o *ListenerServer) Err() error {
	return o.err
}

// SetFds updates the file descriptors served by the server.
//
// A nil slice can be provided to ensure no file descriptors
// are served.
func (o *ListenerServer) SetFds(ctx context.Context, fds []*os.File) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.done:
		return o.err
	case o.fds <- fdsReady{fds: fds}:
		return nil
	}
}

func (o *ListenerServer) loop(ctx context.Context) {
	var currentFds []*os.File
	currentConns := make(map[*net.UnixConn]struct{})

	defer func() {
		if o.err == nil {
			o.err = errors.New("exited due to unknown error")
		}

		for conn := range currentConns {
			conn.Close()
		}

		close(o.done)
	}()

	for {
		select {
		case <-ctx.Done():
			o.err = ctx.Err()
			return
		case fds := <-o.fds:
			currentFds = fds.fds

			o.broadcastFds(currentFds, currentConns)
		case <-o.listener.Done():
			o.err = fmt.Errorf("listener is done - %w", o.listener.Err())
			return
		case conn := <-o.listener.Conns():
			unixConn, ok := conn.(*net.UnixConn)
			if !ok {
				o.err = fmt.Errorf("expected conn to be *net.UnixConn - got %T", conn)
				return
			}

			err := o.sendFdsTo(currentFds, unixConn)
			if err != nil {
				log.Printf("[warn] failed to send current fds to new client - %s", err)

				unixConn.Close()
			} else {
				currentConns[unixConn] = struct{}{}
			}
		}
	}
}

func (o *ListenerServer) broadcastFds(fds []*os.File, currentConns map[*net.UnixConn]struct{}) {
	if len(fds) == 0 {
		return
	}

	for conn := range currentConns {
		err := o.sendFdsTo(fds, conn)
		if err != nil {
			log.Printf("[warn] failed to send current fds to existing client - %s", err)

			delete(currentConns, conn)
		}
	}
}

func (o *ListenerServer) sendFdsTo(fds []*os.File, conn *net.UnixConn) error {
	if len(fds) == 0 {
		return nil
	}

	err := Put(conn, fds...)
	if err != nil {
		conn.Close()

		return err
	}

	return nil
}

type fdsReady struct {
	fds []*os.File
}
