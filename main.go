// snoozled (pronounced "sch noozle dee") is program that helps daemonize
// other programs, specifically programs that use stdin as an interactive
// admin interface.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"time"

	"github.com/stephen-fox/goss"
	"gitlab.com/stephen-fox/snoozled/internal/osspecific"
)

const (
	appName = "snoozled"
	usage   = appName + `

A program that helps daemonize other programs, specifically programs that use
stdin as an interactive admin interface. In 'daemon' mode, it normally creates
a Unix socket or Windows named pipe and streams the child process' IO
to a client. This program can be run in 'client' mode to consume that IO.

The child process' output is also saved to a log file which is automatically
truncated over time.

usage:
  ` + appName + ` daemon [options] <child-program-path> [child-program-args]
  ` + appName + ` client [options] <daemon-socket-path>

options:
`
)

func main() {
	err := runApp()
	if err != nil {
		log.Fatalln(err)
	}
}

func runApp() error {
	displayHelp := flag.Bool("h", false, "Display this information")

	flag.Parse()

	if *displayHelp {
		_, _ = os.Stderr.WriteString(usage)
		flag.PrintDefaults()
		os.Exit(1)
	}

	if flag.NArg() == 0 {
		return errors.New("please specify a mode ('client' or 'daemon') or '-h' for more information")
	}

	fs := flag.NewFlagSet(flag.Arg(0), flag.ExitOnError)

	switch flag.Arg(0) {
	case "client":
		return client(fs)
	case "daemon":
		return daemon(fs)
	default:
		return fmt.Errorf("unknown mode: '%s'", flag.Arg(0))
	}
}

func client(fs *flag.FlagSet) error {
	_ = fs.Parse(os.Args[2:])

	if fs.NArg() == 0 {
		return errors.New("please specify the path to the daemon socket")
	}

	conn, err := goss.Dial(goss.DialConfig{
		Path:    fs.Arg(0),
		Timeout: time.Second,
	})
	if err != nil {
		return fmt.Errorf("failed to open socket - %w", err)
	}
	defer func() {
		_ = conn.Close()
	}()

	ctx, cancelFn := signal.NotifyContext(context.Background(), osspecific.QuitSignals()...)
	defer cancelFn()

	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, os.Stdin)
		done <- err
	}()

	go func() {
		_, err := io.Copy(os.Stdout, conn)
		done <- err
	}()

	select {
	case err = <-done:
		return err
	case <-ctx.Done():
		return nil
	}
}

func daemon(fs *flag.FlagSet) error {
	socketPath := fs.String(
		"l",
		"",
		"The socket path (specify '-' to disable)")
	socketMode := fileMode{
		mode: 0750,
	}
	fs.Var(
		&socketMode,
		"m",
		"The socket's file mode")
	logFilePath := fs.String(
		"o",
		"",
		"The log file path")

	_ = fs.Parse(os.Args[2:])

	if fs.NArg() == 0 {
		return errors.New("please specify an application to execute and its arguments")
	}

	fs.VisitAll(func(f *flag.Flag) {
		if f.Value.String() == "" {
			log.Fatalf("please specify '-%s' - %s", f.Name, f.Usage)
		}
	})

	ctx, cancelFn := signal.NotifyContext(context.Background(), osspecific.QuitSignals()...)
	defer cancelFn()

	logFile, err := startLogFileWriter(ctx, *logFilePath)
	if err != nil {
		return fmt.Errorf("failed to start log file writer - %w", err)
	}
	defer func() {
		_ = logFile.file.Close()
	}()

	child := exec.CommandContext(ctx, fs.Arg(0), fs.Args()[1:]...)

	if *socketPath == "-" {
		child.Stderr = logFile
		child.Stdout = logFile
	} else {
		listener, onNewConns, err := newCtlSocket(ctx, *socketPath, socketMode.mode)
		if err != nil {
			return fmt.Errorf("failed to start ipc listner - %w", err)
		}
		defer func() {
			_ = listener.Close()
		}()

		cm := newConnManager(ctx, onNewConns, logFile)
		defer cm.waitUntilClosed()

		rw := &maybeReaderWriter{
			ctx:     ctx,
			onRead:  cm.readEvents(),
			onWrite: cm.writeEvents(),
		}

		child.Stdin = rw
		child.Stderr = rw
		child.Stdout = rw
	}

	err = child.Run()
	if err != nil {
		return fmt.Errorf("child process exited with error - %w", err)
	}

	log.Println("child process exited without error")

	return nil
}

func newCtlSocket(ctx context.Context, filePath string, mode os.FileMode) (net.Listener, <-chan net.Conn, error) {
	listener, err := goss.Listen(goss.ListenConfig{
		Path:          filePath,
		SystemOptions: osspecific.SocketOptions(mode),
	})
	if err != nil {
		return nil, nil, err
	}

	newConns := make(chan net.Conn)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				log.Printf("failed to accept client - %s", err)
				// TODO: Tell conn manager.
				return
			}

			select {
			case <-ctx.Done():
				return
			case newConns <- conn:
			}
		}
	}()

	return listener, newConns, nil
}

func newConnManager(ctx context.Context, newConns <-chan net.Conn, file *managedFile) *connManager {
	cm := &connManager{
		newConns:  newConns,
		readReqs:  make(chan rwEvent),
		writeReqs: make(chan rwEvent),
		done:      make(chan struct{}),
		file:      file,
	}

	go cm.manageConns(ctx)

	return cm
}

type connManager struct {
	newConns  <-chan net.Conn
	readReqs  chan rwEvent
	writeReqs chan rwEvent
	done      chan struct{}
	file      *managedFile
}

func (o *connManager) waitUntilClosed() {
	<-o.done
}

func (o *connManager) readEvents() chan<- rwEvent {
	return o.readReqs
}

func (o *connManager) writeEvents() chan<- rwEvent {
	return o.writeReqs
}

func (o *connManager) manageConns(ctx context.Context) {
	var currentConn net.Conn
	var queuedConnRead *rwEvent
	closeCurrentConn := make(chan error, 1)

	for {
		select {
		case <-ctx.Done():
			if currentConn != nil {
				_ = currentConn.SetWriteDeadline(time.Now().Add(time.Second))
				_, _ = currentConn.Write([]byte("daemon is shutting down - " +
					ctx.Err().Error() + "\n"))
				_ = currentConn.Close()
			}
			close(o.done)
			return
		case <-closeCurrentConn:
			if currentConn != nil {
				_ = currentConn.Close()
				currentConn = nil
			}
		case newConn := <-o.newConns:
			if currentConn != nil {
				_ = currentConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				_, _ = currentConn.Write([]byte("a new client has connected\n"))
				_ = currentConn.Close()
				currentConn = nil
			}

			_, err := newConn.Write(o.file.buffered())
			if err != nil {
				_ = newConn.Close()
				continue
			}

			if queuedConnRead != nil {
				go asyncRead(newConn, *queuedConnRead, closeCurrentConn)
				queuedConnRead = nil
			}

			currentConn = newConn
		case read := <-o.readReqs:
			if currentConn == nil {
				queuedConnRead = &read
				continue
			}

			if queuedConnRead != nil {
				select {
				case queuedConnRead.cb <- rwEventResult{
					err: errors.New("a read has already been queued"),
				}:
				default:
				}
				queuedConnRead = nil
			}

			go asyncRead(currentConn, read, closeCurrentConn)
		case write := <-o.writeReqs:
			n, err := o.file.Write(write.b)
			write.cb <- rwEventResult{
				n:   n,
				err: err,
			}

			if currentConn != nil {
				_, connErr := currentConn.Write(write.b)
				if connErr != nil {
					_ = currentConn.Close()
					currentConn = nil
				}
			}
		}
	}
}

func asyncRead(reader io.Reader, event rwEvent, onErr chan error) {
	n, err := reader.Read(event.b)
	event.cb <- rwEventResult{
		n: n,
		// Do not send error back to caller because an error
		// may cause the child process to exit, or for the
		// Go standard library to do something to the child
		// process' stdin state.
	}
	if err != nil {
		onErr <- err
	}
}

type maybeReaderWriter struct {
	ctx     context.Context
	onRead  chan<- rwEvent
	onWrite chan<- rwEvent
}

type rwEvent struct {
	b  []byte
	cb chan<- rwEventResult
}

type rwEventResult struct {
	n   int
	err error
}

func (o *maybeReaderWriter) Read(b []byte) (int, error) {
	cb := make(chan rwEventResult, 1)

	select {
	case o.onRead <- rwEvent{
		b:  b,
		cb: cb,
	}:
	case <-o.ctx.Done():
		return 0, o.ctx.Err()
	}

	select {
	case result := <-cb:
		return result.n, result.err
	case <-o.ctx.Done():
		return 0, o.ctx.Err()
	}
}

func (o *maybeReaderWriter) Write(b []byte) (int, error) {
	cb := make(chan rwEventResult, 1)

	select {
	case o.onWrite <- rwEvent{
		b:  b,
		cb: cb,
	}:
	case <-o.ctx.Done():
		return 0, o.ctx.Err()
	}

	select {
	case result := <-cb:
		return result.n, result.err
	case <-o.ctx.Done():
		return 0, o.ctx.Err()
	}
}

func startLogFileWriter(ctx context.Context, filePath string) (*managedFile, error) {
	logFile, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}

	w := &managedFile{
		maxFileBytes: 100_000_000,
		maxBufBytes:  4096,
		buf:          bytes.NewBuffer(nil),
		file:         logFile,
	}

	go w.truncateFileLoop(ctx)

	return w, nil
}

type managedFile struct {
	maxFileBytes int64
	maxBufBytes  int
	mu           sync.RWMutex
	buf          *bytes.Buffer
	file         *os.File
}

func (o *managedFile) truncateFileLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)

loop:
	select {
	case <-ctx.Done():
		ticker.Stop()
		_ = o.file.Close()
		return
	case <-ticker.C:
		info, err := o.file.Stat()
		if err != nil {
			goto loop
		}

		if info.Size() > o.maxFileBytes {
			_ = o.truncate()
		}

		goto loop
	}
}

func (o *managedFile) truncate() error {
	o.mu.Lock()
	defer o.mu.Unlock()

	err := o.file.Truncate(0)
	if err != nil {
		return err
	}

	_, err = o.file.Seek(0, 0)
	if err != nil {
		return err
	}

	return nil
}

func (o *managedFile) buffered() []byte {
	o.mu.Lock()

	b := make([]byte, o.buf.Len())
	_, _ = o.buf.Read(b)

	o.mu.Unlock()
	return b
}

func (o *managedFile) Write(b []byte) (int, error) {
	o.mu.Lock()

	n, err := o.file.Write(b)
	o.buf.Write(b)

	if o.buf.Len() > o.maxBufBytes {
		_, _ = o.buf.Read(make([]byte, len(b)))
	}

	o.mu.Unlock()

	return n, err
}

type fileMode struct {
	mode os.FileMode
}

func (o *fileMode) String() string {
	return fmt.Sprintf("%o (%s)", o.mode, o.mode.String())
}

func (o *fileMode) Set(s string) error {
	i, err := strconv.ParseInt(s, 8, 32)
	if err != nil {
		return err
	}
	o.mode = os.FileMode(i)
	return nil
}
