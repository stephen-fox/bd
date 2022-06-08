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

options:
`
)

var closeLogFn func() error

func main() {
	err := runApp()
	if closeLogFn != nil {
		_ = closeLogFn()
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
		mode: 0750,
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
		"The log file path")

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

		us := exec.Command(os.Args[0], os.Args[1:]...)
		us.Env = os.Environ()
		us.Env = append(us.Env, childEnvName+"=true")
		us.SysProcAttr = childSysProcAttr
		err := us.Start()
		if err != nil {
			return fmt.Errorf("failed to exec to background - %w", err)
		}

		if *pidFilePath != "" {
			_ = os.Remove(*pidFilePath)

			err = os.WriteFile(
				*pidFilePath,
				[]byte(fmt.Sprintf("%d\n", us.Process.Pid)),
				0600)
			if err != nil {
				_ = us.Process.Kill()
				return fmt.Errorf("failed to write pid file '%s' - %w",
					*pidFilePath, err)
			}
		}

		return nil
	}

	ctx, cancelFn := signal.NotifyContext(context.Background(), osspecific.QuitSignals()...)
	defer cancelFn()

	logFile, err := startManagedFile(ctx, *logFilePath)
	if err != nil {
		return fmt.Errorf("failed to start log file writer - %w", err)
	}
	closeLogFn = logFile.file.Close

	log.SetPrefix(fmt.Sprintf("[%s] ", appName))
	log.SetOutput(logFile)

	if *workingDirPath != "" {
		err = os.Chdir(*workingDirPath)
		if err != nil {
			return fmt.Errorf("failed to change current working directory - %w", err)
		}
	}

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

		stdinPipe, err := child.StdinPipe()
		if err != nil {
			return err
		}

		cm := newConnManager(ctx, connManagerConfig{
			newConns: onNewConns,
			file:     logFile,
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

	err = child.Start()
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
				return
			case newConns <- conn:
			}
		}
	}()

	return listener, newConns, nil
}

type connManagerConfig struct {
	newConns <-chan net.Conn
	file     *managedFile
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
	var currentConn net.Conn
	defer func() {
		if currentConn != nil {
			_ = currentConn.SetWriteDeadline(time.Now().Add(time.Second))
			_, _ = currentConn.Write([]byte("daemon is shutting down - " +
				ctx.Err().Error() + "\n"))
			_ = currentConn.Close()
		}
		close(o.wait)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-o.done:
			return
		case newConn := <-o.config.newConns:
			if currentConn != nil {
				_ = currentConn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				_, _ = currentConn.Write([]byte("a new client has connected\n"))
				_ = currentConn.Close()
				currentConn = nil
			}

			_, err := newConn.Write(o.config.file.buffered())
			if err != nil {
				_ = newConn.Close()
				continue
			}

			go io.Copy(o.config.writeTo, newConn)

			currentConn = newConn
		case write := <-o.writeReqs:
			n, err := o.config.file.Write(write.b)
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

func startManagedFile(ctx context.Context, filePath string) (*managedFile, error) {
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

	for {
		select {
		case <-ctx.Done():
			ticker.Stop()
			_ = o.file.Close()
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
