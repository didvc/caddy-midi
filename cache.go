package caddymidi

import (
	"bufio"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sync/singleflight"
)

// cache holds rendered WAV files on disk, keyed by everything that affects the
// output. Synthesis is expensive but perfectly deterministic, so a hit is
// always safe to serve.
//
// Entries are never evicted: a rendered WAV is a pure function of inputs that
// are still on disk, so the cache is a rebuildable artifact directory rather
// than state. Point cache_dir at a tmpfs or sweep it on a timer if that
// matters for the deployment.
type cache struct {
	dir    string
	inWork singleflight.Group
}

// open returns a read-seekable handle to the rendered audio for key, calling
// render to produce it on a miss. Concurrent requests for the same key render
// once and share the result.
func (c *cache) open(key string, render func(io.Writer) error) (*os.File, error) {
	path := filepath.Join(c.dir, key+".wav")

	f, err := os.Open(path)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	_, err, _ = c.inWork.Do(key, func() (any, error) {
		// Another goroutine may have finished while this one was queued.
		if _, err := os.Stat(path); err == nil {
			return nil, nil
		}
		return nil, writeAtomic(c.dir, path, render)
	})
	if err != nil {
		return nil, err
	}

	return os.Open(path)
}

// writeAtomic renders to a temporary file and renames it into place, so an
// aborted or failed render can never leave a truncated WAV behind to be served
// as a cache hit later.
func writeAtomic(dir, path string, render func(io.Writer) error) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".render-*")
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name()) // no-op once the rename below has succeeded
	}()

	w := bufio.NewWriterSize(tmp, 128<<10)
	if err := render(w); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), path)
}

// renderToTemp is the no-cache path. It still renders to a file rather than to
// memory so that responses stay seekable and memory stays bounded; the file is
// unlinked immediately, so the kernel reclaims it when the handle closes.
func renderToTemp(render func(io.Writer) error) (*os.File, error) {
	tmp, err := os.CreateTemp("", "caddy-midi-*.wav")
	if err != nil {
		return nil, err
	}
	if err := os.Remove(tmp.Name()); err != nil {
		tmp.Close()
		return nil, err
	}

	w := bufio.NewWriterSize(tmp, 128<<10)
	if err := render(w); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := w.Flush(); err != nil {
		tmp.Close()
		return nil, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		return nil, err
	}

	return tmp, nil
}
