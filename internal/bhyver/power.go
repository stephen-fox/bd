package bhyver

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"codeberg.org/stephen-fox/bd/internal/lctx"
)

// PowerStateRequestsHandler handles clients accepted by listener until
// the context is canceled or the listener returns an error.
func PowerStateRequestsHandler(ctx context.Context, listener *lctx.ListenerCtx) <-chan *PowerStateRequest {
	requests := make(chan *PowerStateRequest)

	go func() {
		defer close(requests)

		for {
			select {
			case <-ctx.Done():
				log.Printf("power state handler exiting - %s", ctx.Err())

				return
			case <-listener.Done():
				log.Printf("power state listener is done - %s", listener.Err())

				return
			case conn := <-listener.Conns():
				err := handlePowerStateRequest(ctx, requests, conn)
				if err != nil {
					log.Printf("power state handler exiting - %s", err)

					return
				}
			}
		}
	}()

	return requests
}

func handlePowerStateRequest(ctx context.Context, requests chan *PowerStateRequest, conn net.Conn) error {
	defer conn.Close()

	sendMsgFn := func(msg string) error {
		_, err := conn.Write([]byte(msg + "\n"))
		return err
	}

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))

	scanner := bufio.NewScanner(io.LimitReader(conn, 512))

	if !scanner.Scan() {
		return nil
	}

	text := scanner.Text()

	state := PowerStateFromString(text)
	if state == UnknownPowerState {
		sendMsgFn(fmt.Sprintf("error: unknown power state: %q", text))

		return nil
	}

	request := &PowerStateRequest{
		newState: state,
		cb:       make(chan error, 1),
	}

	select {
	case <-ctx.Done():
		sendMsgFn(fmt.Sprintf("error: %s", ctx.Err().Error()))

		return ctx.Err()
	case requests <- request:
	}

	select {
	case <-ctx.Done():
		sendMsgFn(fmt.Sprintf("error: %s", ctx.Err().Error()))

		return ctx.Err()
	case err := <-request.cb:
		if err != nil {
			sendMsgFn(fmt.Sprintf("error: %s", err.Error()))
		} else {
			sendMsgFn("")
		}
	}

	return nil
}

func PowerStateFromString(str string) PowerState {
	switch str {
	case OnPowerState.String():
		return OnPowerState
	case AcpiOffPowerState.String():
		return AcpiOffPowerState
	case AcpiRebootPowerState.String():
		return AcpiRebootPowerState
	case PullPowerCablePowerState.String():
		return PullPowerCablePowerState
	case PullPowerCableRebootPowerState.String():
		return PullPowerCableRebootPowerState
	default:
		return UnknownPowerState
	}
}

type PowerState int

func (o PowerState) String() string {
	switch o {
	case OnPowerState:
		return "on"
	case AcpiOffPowerState:
		return "off"
	case AcpiRebootPowerState:
		return "reboot"
	case PullPowerCablePowerState:
		return "pull-cable"
	case PullPowerCableRebootPowerState:
		return "pull-cable-reboot"
	default:
		return "unknown power state"
	}
}

const (
	UnknownPowerState PowerState = iota
	OnPowerState
	AcpiOffPowerState
	AcpiRebootPowerState
	PullPowerCablePowerState
	PullPowerCableRebootPowerState
)

type PowerStateRequest struct {
	newState PowerState
	cb       chan error
}
