// snoozled (pronounced "sch noozle dee") is a program that helps daemonize
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
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

shared options:
`
)

const (
	noClientFlag uint8 = iota
	bufferedOutputClientFlag
)

var closeLogFn func()

func main() {
	err := runApp()
	if closeLogFn != nil {
		closeLogFn()
	}
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
	waitForSocketToClose := fs.Bool(
		"w",
		false,
		"Do not exit if stdin is closed (useful for writing to stdin in a shell,\n"+
			"closing it, and waiting until the daemon shuts down)")
	noBufferedOutput := fs.Bool(
		"q",
		false,
		"Do not retrieve buffered output from daemon's child process")

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

	var clientFlags byte
	if !*noBufferedOutput {
		clientFlags |= bufferedOutputClientFlag
	}

	_, err = conn.Write([]byte{clientFlags})
	if err != nil {
		return fmt.Errorf("failed to write client flags to socket - %s", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := io.Copy(conn, os.Stdin)
		if !*waitForSocketToClose {
			done <- err
		}
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
	foreground := fs.Bool(
		"f",
		false,
		"Stay in the foreground rather than exec'ing into background")
	socketPath := fs.String(
		"l",
		"",
		"The socket path (specify '-' to disable)")
	socketMode := fileMode{
		mode: 0600,
	}
	fs.Var(
		&socketMode,
		"m",
		"The socket's file mode")
	workingDirPath := fs.String(
		"d",
		"",
		"The working directory to use")
	runAsUser := fs.String(
		"u",
		"",
		"Optionally run as a specific user (only supported on Unix systems)")
	pidFilePath := fs.String(
		"p",
		"",
		"Optionally create a PID file at this file path")
	logFilePath := fs.String(
		"o",
		"",
		"Optionally specify a file path to save child's stderr and stdout to")

	_ = fs.Parse(os.Args[2:])

	if fs.NArg() == 0 {
		return errors.New("please specify an application to execute and its arguments")
	}

	fs.VisitAll(func(f *flag.Flag) {
		if strings.Contains(strings.ToLower(f.Usage), "optional") {
			return
		}

		if f.Value.String() == "" {
			log.Fatalf("please specify '-%s' - %s", f.Name, f.Usage)
		}
	})

	if !path.IsAbs(os.Args[0]) {
		return fmt.Errorf("executable path must be absolute - '%s' is a relative path", os.Args[0])
	}

	const childEnvName = appName + "_" + "child"

	isChild := os.Getenv(childEnvName) != "" || *foreground
	if !isChild {
		var childSysProcAttr *syscall.SysProcAttr
		if *runAsUser != "" {
			var err error
			childSysProcAttr, err = osspecific.SysProcAttrForChildProc(*runAsUser)
			if err != nil {
				return fmt.Errorf("failed to get sys proc attr for user '%s' - %w",
					*runAsUser, err)
			}
		}

		restarted := exec.Command(os.Args[0], os.Args[1:]...)
		restarted.Dir = *workingDirPath
		restarted.Env = os.Environ()
		restarted.Env = append(restarted.Env, childEnvName+"=true")
		restarted.SysProcAttr = childSysProcAttr

		err := restarted.Start()
		if err != nil {
			return fmt.Errorf("failed to exec to background - %w", err)
		}

		if *pidFilePath != "" {
			_ = os.Remove(*pidFilePath)

			err = os.WriteFile(
				*pidFilePath,
				[]byte(fmt.Sprintf("%d\n", restarted.Process.Pid)),
				0600)
			if err != nil {
				_ = restarted.Process.Kill()
				return fmt.Errorf("failed to write pid file '%s' - %w",
					*pidFilePath, err)
			}
		}

		return nil
	}

	ctx, cancelFn := signal.NotifyContext(context.Background(), osspecific.QuitSignals()...)
	defer cancelFn()

	var logFile *managedFile
	if *logFilePath != "" {
		var err error
		logFile, err = startManagedFile(*logFilePath)
		if err != nil {
			return fmt.Errorf("failed to start log file writer - %w", err)
		}

		closeLogFn = logFile.close

		log.SetOutput(logFile)
	}

	log.SetPrefix(fmt.Sprintf("[%s] ", appName))

	child := exec.CommandContext(ctx, fs.Arg(0), fs.Args()[1:]...)

	if *socketPath == "-" {
		if logFile != nil {
			child.Stderr = logFile
			child.Stdout = logFile
		}
	} else {
		listener, onNewConns, err := newCtlSocket(ctx, *socketPath, socketMode.mode)
		if err != nil {
			return fmt.Errorf("failed to start ipc listner - %w", err)
		}
		defer func() {
			_ = listener.Close()
		}()

		stdinPipe, err := child.StdinPipe()
		if err != nil {
			return err
		}

		cm := newConnManager(ctx, connManagerConfig{
			newConns: onNewConns,
			logFile:  logFile,
			writeTo:  stdinPipe,
		})
		defer cm.close()

		writer := &writerProxy{
			ctx:     ctx,
			onWrite: cm.writeEvents(),
		}

		child.Stderr = writer
		child.Stdout = writer
	}

	log.Printf("executing: '%s'...", child.String())

	err := child.Start()
	if err != nil {
		return fmt.Errorf("failed to start child process - %w", err)
	}

	err = child.Wait()
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
				_ = conn.Close()
				return
			case newConns <- conn:
			}
		}
	}()

	return listener, newConns, nil
}

type connManagerConfig struct {
	newConns <-chan net.Conn
	logFile  *managedFile
	writeTo  io.Writer
}

func newConnManager(ctx context.Context, config connManagerConfig) *connManager {
	cm := &connManager{
		config:    config,
		writeReqs: make(chan rwEvent),
		done:      make(chan struct{}),
		wait:      make(chan struct{}),
	}

	go cm.manageConnsLoop(ctx)

	return cm
}

type connManager struct {
	config    connManagerConfig
	writeReqs chan rwEvent
	once      sync.Once
	done      chan struct{}
	wait      chan struct{}
}

func (o *connManager) close() {
	o.once.Do(func() {
		close(o.done)
		<-o.wait
	})
}

func (o *connManager) writeEvents() chan<- rwEvent {
	return o.writeReqs
}

func (o *connManager) manageConnsLoop(ctx context.Context) {
	currentConns := make(map[net.Conn]struct{})
	defer func() {
		for conn := range currentConns {
			_ = conn.SetWriteDeadline(time.Now().Add(time.Second))

			_, _ = conn.Write([]byte("daemon is shutting down - " +
				ctx.Err().Error() + "\n"))

			_ = conn.Close()
			delete(currentConns, conn)
		}

		close(o.wait)
	}()

	closeConns := make(chan net.Conn)

	for {
		select {
		case <-ctx.Done():
			return
		case <-o.done:
			return
		case newConn := <-o.config.newConns:
			setDeadLineErr := newConn.SetReadDeadline(time.Now().Add(time.Second))
			if setDeadLineErr == nil {
				options := make([]byte, 1)

				_, _ = newConn.Read(options)

				if o.config.logFile != nil && options[0]&bufferedOutputClientFlag != 0 {
					_, err := newConn.Write(o.config.logFile.buffered())
					if err != nil {
						_ = newConn.Close()
						continue
					}
				}

				_ = newConn.SetReadDeadline(time.Time{})
			}

			go func() {
				_, _ = io.Copy(o.config.writeTo, newConn)
				select {
				case closeConns <- newConn:
				case <-ctx.Done():
					_ = newConn.Close()
				}
			}()

			currentConns[newConn] = struct{}{}
		case closeThis := <-closeConns:
			_ = closeThis.Close()
			delete(currentConns, closeThis)
		case write := <-o.writeReqs:
			var n int
			var err error
			if o.config.logFile != nil {
				n, err = o.config.logFile.Write(write.b)
			} else {
				n = len(write.b)
			}

			write.cb <- rwEventResult{
				n:   n,
				err: err,
			}

			for conn := range currentConns {
				_, connErr := conn.Write(write.b)
				if connErr != nil {
					_ = conn.Close()
					delete(currentConns, conn)
				}
			}
		}
	}
}

type writerProxy struct {
	ctx     context.Context
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

func (o *writerProxy) Write(b []byte) (int, error) {
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

func startManagedFile(filePath string) (*managedFile, error) {
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}

	w := &managedFile{
		maxFileBytes: 100_000_000,
		maxBufBytes:  4096,
		buf:          bytes.NewBuffer(nil),
		file:         file,
		done:         make(chan struct{}),
		wait:         make(chan struct{}),
	}

	go w.truncateFileLoop()

	return w, nil
}

type managedFile struct {
	maxFileBytes int64
	maxBufBytes  int
	mu           sync.Mutex
	buf          *bytes.Buffer
	file         *os.File
	once         sync.Once
	done         chan struct{}
	wait         chan struct{}
}

func (o *managedFile) close() {
	o.once.Do(func() {
		close(o.done)
		<-o.wait
	})
}

func (o *managedFile) truncateFileLoop() {
	ticker := time.NewTicker(time.Hour)

	for {
		select {
		case <-o.done:
			ticker.Stop()
			_ = o.file.Close()
			close(o.wait)
			return
		case <-ticker.C:
			info, err := o.file.Stat()
			if err != nil {
				continue
			}

			if info.Size() > o.maxFileBytes {
				_ = o.truncate()
			}
		}
	}
}

func (o *managedFile) truncate() error {
	o.mu.Lock()
	defer o.mu.Unlock()

	err := o.file.Truncate(0)
	if err != nil {
		return err
	}

	_, err = o.file.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}

	return nil
}

func (o *managedFile) buffered() []byte {
	o.mu.Lock()
	defer o.mu.Unlock()

	b := make([]byte, o.buf.Len())
	_, _ = o.buf.Read(b)

	return b
}

func (o *managedFile) Write(b []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	n, err := o.file.Write(b)
	o.buf.Write(b)

	if o.buf.Len() > o.maxBufBytes {
		_, _ = o.buf.Read(make([]byte, len(b)))
	}

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
