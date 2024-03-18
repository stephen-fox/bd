// Package writerserver provides a server that serves any writes made using
// its Write method to existing clients. Writes are buffered so that new
// clients will receive the buffered data when they first connect.
//
// Connected clients can write data to the server, which is then written to
// a designated io.Writer.
package writerserver
