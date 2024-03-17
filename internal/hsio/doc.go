// Package hsio provides hot-swappable implementations of the interfaces found
// in the io library. These implementations allow callers to swap out the
// underlying io.Reader and io.Writer implementations without interrupting
// an existing read or write operation.
package hsio
