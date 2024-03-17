package hsio

// NewReadWriteCloser instantiates a ReadWriteCloser.
func NewReadWriteCloser() *ReadWriteCloser {
	rw := &ReadWriteCloser{
		reader: NewReadCloser(),
		writer: NewWriteCloser(),
	}

	return rw
}

// ReadWriteCloser implements a hot-swappable io.ReadWriteCloser.
type ReadWriteCloser struct {
	reader *ReadCloser
	writer *WriteCloser
	err    error
}

// Close closes the ReadWriteCloser.
func (o *ReadWriteCloser) Close() error {
	err0 := o.reader.Close()

	err1 := o.writer.Close()

	if err0 != nil {
		return err0
	}

	if err1 != nil {
		return err1
	}

	return nil
}

func (o *ReadWriteCloser) Read(b []byte) (int, error) {
	return o.reader.Read(b)
}

func (o *ReadWriteCloser) Write(b []byte) (int, error) {
	return o.writer.Write(b)
}
