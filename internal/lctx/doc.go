// Package lctx provides a more async-friendly wrapper for the net.Listener
// type. It implements some of the context.Context interface's methods and
// provides a channel for receiving newly-accepted net.Conn clients.
package lctx
