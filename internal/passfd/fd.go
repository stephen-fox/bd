package passfd

// This code is copied from:
// github.com/ftrvxmtrx/fd
//
// I modified it to support the syscall.Socketpair function.
//
// LICENSE:
//
// Copyright © 2012 Serge Zirukin
//
// Permission is hereby granted, free of charge, to any person obtaining a copy of
// this software and associated documentation files (the "Software"), to deal in
// the Software without restriction, including without limitation the rights to
// use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
// the Software, and to permit persons to whom the Software is furnished to do so,
// subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS
// FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR
// COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER
// IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
// CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
//
// Package fd provides a simple API to pass file descriptors
// between different OS processes.
//
// It can be useful if you want to inherit network connections
// from another process without closing them.
//
// Example scenario:
//
//   1) Running server receives a "let's upgrade" message
//   2) Server opens a Unix domain socket for the "upgrade"
//   3) Server starts a new copy of itself and passes Unix domain socket name
//   4) New copy starts reading for the socket
//   5) Server sends its state over the socket, also sending the number
//      of network connections to inherit, then it sends those connections
//      using fd.Put()
//   6) New copy reads the state and inherits connections using fd.Get(),
//      checks that everything is OK and sends the "OK" message to the socket
//   7) Server receives "OK" message and kills itself

import (
	"os"
	"syscall"
)

// UnixConn represents a net.UnixConn.
type UnixConn interface {
	// File returns a copy of the underlying os.File.
	// It is the caller's responsibility to close f when finished.
	// Closing c does not affect f, and closing f does not affect c.
	//
	// The returned os.File's file descriptor is different from
	// the connection's. Attempting to change properties of the
	// original using this duplicate may or may not have the
	// desired effect.
	File() (*os.File, error)

	// Close closes the connection.
	Close() error
}

// Get receives file descriptors from a Unix domain socket.
//
// Num specifies the expected number of file descriptors in one message.
// Internal files' names to be assigned are specified via optional filenames
// argument.
//
// You need to close all files in the returned slice. The slice can be
// non-empty even if this function returns an error.
//
// Use net.FileConn() if you're receiving a network connection.
func Get(via UnixConn, num int, filenames []string) ([]*os.File, error) {
	if num < 1 {
		return nil, nil
	}

	// get the underlying socket
	viaf, err := via.File()
	if err != nil {
		return nil, err
	}
	socket := int(viaf.Fd())
	defer viaf.Close()

	// recvmsg
	buf := make([]byte, syscall.CmsgSpace(num*4))
	_, _, _, _, err = syscall.Recvmsg(socket, nil, buf, 0)
	if err != nil {
		return nil, err
	}

	// parse control msgs
	var msgs []syscall.SocketControlMessage
	msgs, err = syscall.ParseSocketControlMessage(buf)

	// convert fds to files
	res := make([]*os.File, 0, len(msgs))
	for i := 0; i < len(msgs) && err == nil; i++ {
		var fds []int
		fds, err = syscall.ParseUnixRights(&msgs[i])

		for fi, fd := range fds {
			var filename string
			if fi < len(filenames) {
				filename = filenames[fi]
			}

			res = append(res, os.NewFile(uintptr(fd), filename))
		}
	}

	return res, err
}

// Put sends file descriptors to Unix domain socket.
//
// Please note that the number of descriptors in one message is limited
// and is rather small.
// Use conn.File() to get a file if you want to put a network connection.
func Put(via UnixConn, files ...*os.File) error {
	if len(files) == 0 {
		return nil
	}

	viaf, err := via.File()
	if err != nil {
		return err
	}
	socket := int(viaf.Fd())
	defer viaf.Close()

	fds := make([]int, len(files))
	for i := range files {
		fds[i] = int(files[i].Fd())
	}

	rights := syscall.UnixRights(fds...)
	return syscall.Sendmsg(socket, nil, rights, nil, 0)
}
