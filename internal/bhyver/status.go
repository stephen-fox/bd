package bhyver

import (
	"context"
	"fmt"
	"log"
	"net"

	"codeberg.org/stephen-fox/bd/internal/lctx"
)

// StatusRequestsHandler handles clients accepted by listener until
// the context is canceled or the listener returns an error.
func StatusRequestsHandler(ctx context.Context, listener *lctx.ListenerCtx) <-chan *StatusRequest {
	requests := make(chan *StatusRequest)

	go func() {
		defer close(requests)

		for {
			select {
			case <-ctx.Done():
				log.Printf("status handler exiting - %s", ctx.Err())

				return
			case <-listener.Done():
				log.Printf("status listener is done - %s", listener.Err())

				return
			case conn := <-listener.Conns():
				err := handleStatusRequest(ctx, requests, conn)
				if err != nil {
					log.Printf("status handler exiting - %s", err)

					return
				}
			}
		}
	}()

	return requests
}

func handleStatusRequest(ctx context.Context, requests chan *StatusRequest, conn net.Conn) error {
	defer conn.Close()

	sendMsgFn := func(msg string) error {
		_, err := conn.Write([]byte(msg + "\n"))
		return err
	}

	request := &StatusRequest{
		cb: make(chan error, 1),
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
			sendMsgFn(request.status)
		}
	}

	return nil
}

type StatusRequest struct {
	status string
	cb     chan error
}
