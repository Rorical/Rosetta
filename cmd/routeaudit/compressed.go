package main

import (
	"compress/gzip"
	"io"
	"os"
	"strings"
)

type captureReader struct {
	io.Reader
	file *os.File
	gzip *gzip.Reader
}

func (r *captureReader) Close() error {
	if r.gzip != nil {
		r.gzip.Close()
	}
	return r.file.Close()
}
func openCapture(path string) (*captureReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r := &captureReader{Reader: f, file: f}
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		r.Reader = gz
		r.gzip = gz
	}
	return r, nil
}
