package smirror

import (
	"bytes"
	"compress/gzip"
	"io"
	"smirror/config"
)

//OnClose represents on close writer listener
type OnClose func(writer *Writer) error

//Writer represents a writer
type Writer struct {
	io.WriteCloser
	Reader     io.Reader
	route      *config.Route
	buffer     *bytes.Buffer
	gzipWriter *gzip.Writer
	listener   OnClose
}

//NewWriter returns a route writer
func NewWriter(route *config.Route, listener OnClose) io.WriteCloser {
	buffer := new(bytes.Buffer)
	result := &Writer{
		WriteCloser: WriteNopCloser(buffer),
		buffer:      buffer,
		listener:    listener,
	}
	if route.Compression != nil {
		if route.Codec == config.GZipCodec {
			result.gzipWriter = gzip.NewWriter(buffer)
			result.WriteCloser = result.gzipWriter
		}
	}
	return result
}

//Close closes writer and notifies listener
func (w *Writer) Close() error {
	if w.gzipWriter != nil {
		if err := w.gzipWriter.Flush(); err != nil {
			return err
		}
	}
	if err := w.WriteCloser.Close(); err != nil {
		return err
	}
	w.Reader = w.buffer
	return w.listener(w)
}

type writeNopCloser struct {
	io.Writer
}

func (writeNopCloser) Close() error { return nil }

// WriteNopCloser returns a WriteCloser with a no-op Close method wrapping
// the provided Writer w.
func WriteNopCloser(w io.Writer) io.WriteCloser {
	return writeNopCloser{w}
}
