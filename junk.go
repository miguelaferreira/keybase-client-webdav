package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"sync"
	"time"

	"golang.org/x/net/webdav"
)

// junkStore holds Finder metadata files (._* and .DS_Store) in memory so they
// never reach KBFS.
type junkStore struct {
	mu    sync.Mutex
	files map[string]*junkFile
}

type junkFile struct {
	data  []byte
	mtime time.Time
}

func newJunkStore() *junkStore { return &junkStore{files: map[string]*junkFile{}} }

func (s *junkStore) stat(name string) (fs.FileInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.files[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return &kbInfo{name: path.Base(name), size: int64(len(f.data)), mtime: f.mtime}, nil
}

func (s *junkStore) remove(name string) {
	s.mu.Lock()
	delete(s.files, name)
	s.mu.Unlock()
}

func (s *junkStore) rename(from, to string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.files[from]
	if !ok {
		return fs.ErrNotExist
	}
	delete(s.files, from)
	s.files[to] = f
	return nil
}

func (s *junkStore) open(name string, flag int) (webdav.File, error) {
	s.mu.Lock()
	f, ok := s.files[name]
	if !ok {
		if flag&os.O_CREATE == 0 {
			s.mu.Unlock()
			return nil, fs.ErrNotExist
		}
		f = &junkFile{mtime: time.Now()}
		s.files[name] = f
	}
	if flag&os.O_TRUNC != 0 {
		f.data = nil
	}
	s.mu.Unlock()
	return &junkHandle{store: s, name: name, f: f}, nil
}

type junkHandle struct {
	store *junkStore
	name  string
	f     *junkFile
	off   int64
}

func (h *junkHandle) Close() error { return nil }

func (h *junkHandle) Read(p []byte) (int, error) {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	if h.off >= int64(len(h.f.data)) {
		return 0, io.EOF
	}
	n := copy(p, h.f.data[h.off:])
	h.off += int64(n)
	return n, nil
}

func (h *junkHandle) Write(p []byte) (int, error) {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	if end := h.off + int64(len(p)); end > int64(len(h.f.data)) {
		h.f.data = append(h.f.data, bytes.Repeat([]byte{0}, int(end)-len(h.f.data))...)
	}
	copy(h.f.data[h.off:], p)
	h.off += int64(len(p))
	h.f.mtime = time.Now()
	return len(p), nil
}

func (h *junkHandle) Seek(offset int64, whence int) (int64, error) {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	switch whence {
	case io.SeekStart:
		h.off = offset
	case io.SeekCurrent:
		h.off += offset
	case io.SeekEnd:
		h.off = int64(len(h.f.data)) + offset
	}
	if h.off < 0 {
		h.off = 0
		return 0, errors.New("negative offset")
	}
	return h.off, nil
}

func (h *junkHandle) Stat() (fs.FileInfo, error) { return h.store.stat(h.name) }
func (h *junkHandle) Readdir(int) ([]fs.FileInfo, error) {
	return nil, errors.New("not a directory")
}
func (h *junkHandle) ContentType(context.Context) (string, error) {
	return "application/octet-stream", nil
}
