package app

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func console(flagSet *flag.FlagSet) error {
	singleVmMode := flagSet.Bool(
		singleVmModeArg,
		false,
		singleVmModeDesc)

	allowStdinToClose := flagSet.Bool(
		"w",
		false,
		"Do not exit if stdin is closed (useful for writing to stdin in a shell,\n"+
			"closing it, and waiting until the daemon shuts down)")

	_ = flagSet.Parse(os.Args[2:])

	// TODO: Maybe some day allow this for non-root users.
	err := checkIfRunningAsRoot()
	if err != nil {
		return err
	}

	vmName, err := getVmFromFlagsOrSingleVmMode(flagSet, singleVmMode)
	if err != nil {
		return err
	}

	vmDirPath := vmRuntimeDirPath(vmName)

	conn, err := net.Dial("unix", consoleSocketPath(vmDirPath))
	if err != nil {
		return fmt.Errorf("failed to open console unix socket - %w", err)
	}
	defer conn.Close()

	if os.Getuid() == 0 {
		runAsUID, runAsGID, err := lookupUser("nobody")
		if err != nil {
			return fmt.Errorf("failed to lookup nobody user - %w", err)
		}

		chrootDirPath := "/var/empty"
		err = syscall.Chroot(chrootDirPath)
		if err != nil {
			return fmt.Errorf("failed to chroot to %q - %w",
				chrootDirPath, err)
		}

		err = dropPrivsToUser(int(runAsUID), int(runAsGID))
		if err != nil {
			return fmt.Errorf("failed to drop privs to uid %d gid %d - %w",
				runAsUID, runAsGID, err)
		}
	}

	previousTermState, err := term.MakeRaw(0)
	if err != nil {
		return fmt.Errorf("failed to put terminal into raw mode - %w", err)
	}
	defer term.Restore(0, previousTermState)

	err = unix.CapEnter()
	if err != nil {
		return fmt.Errorf("failed to enter capability mode - %w", err)
	}

	errs := make(chan error, 2)

	go func() {
		err := copyStdinToConsole(conn)
		if err != nil {
			errs <- err
			return
		}

		if !*allowStdinToClose {
			errs <- nil
		}
	}()

	go func() {
		_, err := io.Copy(os.Stdout, conn)
		errs <- err
	}()

	// I prefer seeing the exact reason compared to "context canceled".
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	defer signal.Stop(signals)

	select {
	case s := <-signals:
		return fmt.Errorf("received signal: %s / %d", s.String(), s)
	case err = <-errs:
		return err
	}
}

func dropPrivsToUser(uid int, gid int) error {
	err := unix.Setresgid(gid, gid, gid)
	if err != nil {
		return fmt.Errorf("setresgid failed for gid %d - %w", gid, err)
	}

	err = unix.Setresuid(uid, uid, uid)
	if err != nil {
		return fmt.Errorf("setresuid failed for uid %d - %w", uid, err)
	}

	return nil
}

func copyStdinToConsole(conn io.Writer) error {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Split(bufio.ScanBytes)

	// From logging: 0xd 0d 7e 2e
	// \n~.
	// Apparently, \n is 0x09.
	const stopTerminalEscSeq uint32 = 0x0d_7e_2e_00
	const startOfEscSeq uint32 = 0x0d_7e_00_00

	var lastFourBytes uint32
	var escSeqStarted bool

	for scanner.Scan() {
		b := scanner.Bytes()[0]

		// 0xaa41bb42 becomes 0x41bb4200.
		lastFourBytes <<= 8
		// 0x41bb4200 becomes 0x41bb42ff where ff is the new byte.
		lastFourBytes |= uint32(b)

		switch {
		case (lastFourBytes<<16)^startOfEscSeq == 0:
			// Do not send the "~" until the user sends the next byte.
			escSeqStarted = true
			continue
		case (lastFourBytes<<8)^stopTerminalEscSeq == 0:
			return nil
		case escSeqStarted:
			// The next byte was not ".", send "~".
			escSeqStarted = false

			_, err := conn.Write([]byte{0x7e})
			if err != nil {
				return fmt.Errorf("failed to write to conn - %w", err)
			}
		}

		_, err := conn.Write(scanner.Bytes())
		if err != nil {
			return fmt.Errorf("failed to write to conn - %w", err)
		}
	}

	return scanner.Err()
}
