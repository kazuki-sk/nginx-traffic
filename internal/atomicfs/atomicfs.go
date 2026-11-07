// Package atomicfs writes files atomically (temp file → fsync → rename).
package atomicfs

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
)

// WriteFile writes data to path atomically with the given mode. The
// temporary file is created in the same directory so rename(2) is atomic;
// the directory is fsynced afterwards so the rename is durable.
func WriteFile(path string, data []byte, mode os.FileMode) error {
	return WriteFunc(path, mode, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}

// WriteFunc is WriteFile with a streaming producer.
func WriteFunc(path string, mode os.FileMode, produce func(io.Writer) error) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			os.Remove(tmp)
		}
	}()
	bw := bufio.NewWriterSize(f, 64<<10)
	if err = produce(bw); err == nil {
		err = bw.Flush()
	}
	if err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmp, mode); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	if d, derr := os.Open(dir); derr == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
