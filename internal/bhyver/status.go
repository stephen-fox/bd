package bhyver

import (
	"context"
	"net"
	"time"
)

func (o *Runner) handleStatusClient(_ context.Context, conn net.Conn) error {
	defer conn.Close()

	sendMsgFn := func(msg string) error {
		_ = conn.SetDeadline(time.Now().Add(time.Second))

		_, err := conn.Write([]byte(msg + "\n"))

		_ = conn.SetDeadline(time.Time{})

		return err
	}

	for {
		request := &StatusRequest{
			cb: make(chan struct{}, 1),
		}

		select {
		case <-o.done:
			sendMsgFn(runnerStoppedMsg)

			return o.Err()
		case o.statusRequests <- request:
			// Keep going.
		}

		select {
		case <-o.done:
			sendMsgFn(runnerStoppedMsg)

			return o.Err()
		case <-request.cb:
			err := sendMsgFn(request.status)
			if err != nil {
				return nil
			}
		}

		time.Sleep(500 * time.Millisecond)
	}
}

type StatusRequest struct {
	status string
	cb     chan struct{}
}
