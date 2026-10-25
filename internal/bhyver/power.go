package bhyver

import (
	"bufio"
	"context"
	"io"
	"net"
	"time"
)

func (o *Runner) handlePowerClient(ctx context.Context, conn net.Conn) error {
	defer conn.Close()

	sendMsgFn := func(msg string) error {
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))

		_, err := conn.Write([]byte(msg + "\n"))
		return err
	}

	_ = conn.SetReadDeadline(time.Now().Add(time.Second))

	scanner := bufio.NewScanner(io.LimitReader(conn, 512))

	if !scanner.Scan() {
		return nil
	}

	text := scanner.Text()

	state := PowerStateFromString(text)
	if state == UnknownPowerState {
		sendMsgFn(runnerErrorPrefix + "unknown power state requested")

		return nil
	}

	request := &PowerStateRequest{
		newState: state,
		cb:       make(chan error, 1),
	}

	select {
	case <-o.done:
		sendMsgFn(runnerStoppedMsg)

		return ctx.Err()
	case o.powerRequests <- request:
		// Keep going.
	}

	select {
	case <-o.done:
		sendMsgFn(runnerStoppedMsg)

		return ctx.Err()
	case err := <-request.cb:
		if err != nil {
			sendMsgFn(runnerErrorPrefix + err.Error())
		} else {
			sendMsgFn("")
		}
	}

	return nil
}

func PowerStateFromString(str string) PowerState {
	switch str {
	case AcpiOffPowerState.String():
		return AcpiOffPowerState
	case PullPowerCablePowerState.String():
		return PullPowerCablePowerState
	default:
		return UnknownPowerState
	}
}

type PowerState int

func (o PowerState) String() string {
	switch o {
	case AcpiOffPowerState:
		return "off"
	case PullPowerCablePowerState:
		return "pull-cable"
	default:
		return "unknown power state"
	}
}

const (
	UnknownPowerState PowerState = iota
	AcpiOffPowerState
	PullPowerCablePowerState
)

type PowerStateRequest struct {
	newState PowerState
	cb       chan error
}
