// Package fdserver provides a server that sends file descriptors (*os.File)
// to clients using a Unix socket.
//
// If the server is updated with new file descriptors, it will send them to
// any client that is currently connected. This allows clients to receive
// updated file descriptors or additional file descriptors depending on the
// needs to the application.
package fdserver
