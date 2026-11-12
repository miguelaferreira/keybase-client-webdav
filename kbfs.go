package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"
)

// kbfsRootDirs are the children of the virtual root served at "/". The Keybase
// service refuses to list /keybase itself ("RootFS cannot chroot"), so its three children are
// synthesized.
var kbfsRootDirs = []string{"private", "public", "team"}

// keybaseCLI runs `keybase fs ...` commands. It is the only thing that talks to
// the Keybase service, so the bridge needs no kernel extension and keeps
// working across macOS and Keybase upgrades as long as the CLI does.
type keybaseCLI struct {
	bin string
	sem chan struct{}
	env []string
}

// opTimeout bounds every keybase call except uploads, so a wedged call cannot
// hold a process slot (and queue every other request) indefinitely.
const opTimeout = 60 * time.Second

// childEnv returns the environment for keybase processes with PATH replaced by
// system directories plus the directory of the keybase binary. The inherited
// PATH may contain directories on this mount; a lookup through one would call
// back into this server.
func childEnv(bin string) []string {
	env := []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin:" + path.Dir(bin)}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "PATH=") {
			env = append(env, e)
		}
	}
	return env
}

func newKeybaseCLI(bin string, maxProcs int) *keybaseCLI {
	return &keybaseCLI{bin: bin, sem: make(chan struct{}, maxProcs), env: childEnv(bin)}
}

func (k *keybaseCLI) acquire(ctx context.Context) error {
	select {
	case k.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (k *keybaseCLI) release() { <-k.sem }

// run executes `keybase fs <args>` and returns stdout. The CLI does not
// reliably set its exit status (ls exits 0 on error), so stderr is also
// inspected.
func (k *keybaseCLI) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	if args[0] != "write" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opTimeout)
		defer cancel()
	}
	if err := k.acquire(ctx); err != nil {
		return nil, err
	}
	defer k.release()
	cmd := exec.CommandContext(ctx, k.bin, append([]string{"fs"}, args...)...)
	cmd.Env = k.env
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if cerr := classifyStderr(stderr.String()); cerr != nil {
		return nil, cerr
	}
	if err != nil {
		return nil, fmt.Errorf("keybase fs %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func classifyStderr(s string) error {
	switch {
	case strings.Contains(s, "does not exist"):
		return fs.ErrNotExist
	case strings.Contains(s, "already exists"):
		return fs.ErrExist
	case strings.Contains(s, "ERRO"):
		return errors.New(strings.TrimSpace(s))
	}
	return nil
}

// kbPath maps a WebDAV path to the KBFS path the CLI expects.
func kbPath(name string) string {
	return path.Join("/keybase", path.Clean("/"+name))
}

type kbInfo struct {
	name  string
	size  int64
	mtime time.Time
	dir   bool
}

func (i *kbInfo) Name() string { return i.name }
func (i *kbInfo) Size() int64  { return i.size }
func (i *kbInfo) ModTime() time.Time {
	return i.mtime
}
func (i *kbInfo) IsDir() bool { return i.dir }
func (i *kbInfo) Sys() any    { return nil }
func (i *kbInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}

func (i *kbInfo) etag() string {
	return fmt.Sprintf(`"%x%x"`, i.mtime.UnixNano(), i.size)
}

func virtualDir(name string) *kbInfo {
	return &kbInfo{name: name, dir: true, mtime: time.Unix(0, 0)}
}

// parseStat parses `keybase fs stat` output: a path line followed by
// "<time>\t<TYPE>\t<size>\t<name>\t<owner>\t<status>".
func parseStat(out []byte, name string) (*kbInfo, error) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("unexpected stat output %q", out)
	}
	f := strings.Split(lines[1], "\t")
	if len(f) < 3 {
		return nil, fmt.Errorf("unexpected stat line %q", lines[1])
	}
	mt, err := time.ParseInLocation("2006-01-02 15:04:05 MST", f[0], time.Local)
	if err != nil || mt.IsZero() {
		mt = time.Unix(0, 0)
	}
	size, _ := strconv.ParseInt(f[2], 10, 64)
	return &kbInfo{name: path.Base(name), size: size, mtime: mt, dir: f[1] == "DIR"}, nil
}

var lsLine = regexp.MustCompile(`^([dl-])[rwx-]{9}\s+(\d+)\s+(\w{3})\s+(\d{1,2})\s+(\d{4}|\d{2}:\d{2}) (.+)$`)

// parseLs parses `keybase fs ls -l --nocolor` output. Dates carry no time zone
// and, for files older than about six months, no time of day.
func parseLs(out []byte, now time.Time) []fs.FileInfo {
	var infos []fs.FileInfo
	for _, line := range strings.Split(string(out), "\n") {
		m := lsLine.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		size, _ := strconv.ParseInt(m[2], 10, 64)
		infos = append(infos, &kbInfo{
			name:  m[6],
			size:  size,
			mtime: parseLsDate(m[3], m[4], m[5], now),
			dir:   m[1] == "d",
		})
	}
	return infos
}

func parseLsDate(mon, day, yearOrTime string, now time.Time) time.Time {
	if strings.Contains(yearOrTime, ":") {
		t, err := time.ParseInLocation("Jan 2 15:04 2006", fmt.Sprintf("%s %s %s %d", mon, day, yearOrTime, now.Year()), time.Local)
		if err != nil {
			return time.Unix(0, 0)
		}
		if t.After(now.Add(24 * time.Hour)) {
			t = t.AddDate(-1, 0, 0)
		}
		return t
	}
	t, err := time.ParseInLocation("Jan 2 2006", fmt.Sprintf("%s %s %s", mon, day, yearOrTime), time.Local)
	if err != nil {
		return time.Unix(0, 0)
	}
	return t
}

// statCache remembers the entries returned by directory listings for a few
// seconds. The WebDAV handler re-opens every child of a listed directory to
// read its properties; without the cache each of those is a `keybase fs stat`
// process.
type statCache struct {
	mu      sync.Mutex
	entries map[string]cachedStat
}

type cachedStat struct {
	info    *kbInfo
	expires time.Time
}

const statCacheTTL = 5 * time.Second

func (c *statCache) get(name string) (*kbInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[name]
	if !ok || time.Now().After(e.expires) {
		delete(c.entries, name)
		return nil, false
	}
	return e.info, true
}

func (c *statCache) put(name string, info *kbInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]cachedStat{}
	}
	c.entries[name] = cachedStat{info: info, expires: time.Now().Add(statCacheTTL)}
}

// invalidate drops name and everything under it.
func (c *statCache) invalidate(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if k == name || strings.HasPrefix(k, name+"/") {
			delete(c.entries, k)
		}
	}
}

// kbfsFS implements webdav.FileSystem on top of the keybase CLI. Finder
// metadata files (._* and .DS_Store) are kept in memory and never written to
// KBFS.
type kbfsFS struct {
	cli      *keybaseCLI
	readOnly bool
	junk     *junkStore
	cache    statCache
}

func newKBFS(cli *keybaseCLI, readOnly bool) *kbfsFS {
	return &kbfsFS{cli: cli, readOnly: readOnly, junk: newJunkStore()}
}

func isJunk(name string) bool {
	b := path.Base(name)
	return b == ".DS_Store" || strings.HasPrefix(b, "._")
}

func (f *kbfsFS) Stat(ctx context.Context, name string) (fs.FileInfo, error) {
	c := path.Clean("/" + name)
	if c == "/" {
		return virtualDir("/"), nil
	}
	for _, d := range kbfsRootDirs {
		if c == "/"+d {
			return virtualDir(d), nil
		}
	}
	if isJunk(c) {
		return f.junk.stat(c)
	}
	if info, ok := f.cache.get(c); ok {
		return info, nil
	}
	out, err := f.cli.run(ctx, nil, "stat", kbPath(c))
	if err != nil {
		return nil, err
	}
	info, err := parseStat(out, c)
	if err != nil {
		return nil, err
	}
	f.cache.put(c, info)
	return info, nil
}

func (f *kbfsFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	if f.readOnly {
		return fs.ErrPermission
	}
	c := path.Clean("/" + name)
	if _, err := f.Stat(ctx, c); err == nil {
		return fs.ErrExist
	}
	f.cache.invalidate(c)
	_, err := f.cli.run(ctx, nil, "mkdir", kbPath(c))
	return err
}

func (f *kbfsFS) RemoveAll(ctx context.Context, name string) error {
	if f.readOnly {
		return fs.ErrPermission
	}
	c := path.Clean("/" + name)
	if isJunk(c) {
		f.junk.remove(c)
		return nil
	}
	f.cache.invalidate(c)
	_, err := f.cli.run(ctx, nil, "rm", "-r", kbPath(c))
	return err
}

func (f *kbfsFS) Rename(ctx context.Context, oldName, newName string) error {
	if f.readOnly {
		return fs.ErrPermission
	}
	o, n := path.Clean("/"+oldName), path.Clean("/"+newName)
	if isJunk(o) || isJunk(n) {
		return f.junk.rename(o, n)
	}
	f.cache.invalidate(o)
	f.cache.invalidate(n)
	_, err := f.cli.run(ctx, nil, "mv", "-f", kbPath(o), kbPath(n))
	return err
}

func (f *kbfsFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	c := path.Clean("/" + name)
	writing := flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0
	if isJunk(c) {
		return f.junk.open(c, flag)
	}
	if writing && f.readOnly {
		return nil, fs.ErrPermission
	}

	info, err := f.Stat(ctx, c)
	if err != nil && !(errors.Is(err, fs.ErrNotExist) && flag&os.O_CREATE != 0) {
		return nil, err
	}
	if info != nil && info.IsDir() {
		return &kbDir{fs: f, name: c, info: info.(*kbInfo)}, nil
	}
	if info != nil && flag&os.O_EXCL != 0 && flag&os.O_CREATE != 0 {
		return nil, fs.ErrExist
	}
	if !writing {
		return &kbReader{fs: f, name: c, info: info.(*kbInfo), ctx: ctx}, nil
	}
	return newKBWriter(ctx, f, c, info, flag)
}

// kbDir is a directory handle.
type kbDir struct {
	fs      *kbfsFS
	name    string
	info    *kbInfo
	entries []fs.FileInfo
	loaded  bool
	pos     int
}

func (d *kbDir) Close() error                         { return nil }
func (d *kbDir) Read([]byte) (int, error)             { return 0, errors.New("is a directory") }
func (d *kbDir) Write([]byte) (int, error)            { return 0, errors.New("is a directory") }
func (d *kbDir) Seek(int64, int) (int64, error)       { return 0, errors.New("is a directory") }
func (d *kbDir) Stat() (fs.FileInfo, error)           { return d.info, nil }
func (d *kbDir) ETag(context.Context) (string, error) { return d.info.etag(), nil }

func (d *kbDir) Readdir(count int) ([]fs.FileInfo, error) {
	if !d.loaded {
		if d.name == "/" {
			for _, n := range kbfsRootDirs {
				d.entries = append(d.entries, virtualDir(n))
			}
		} else {
			out, err := d.fs.cli.run(context.Background(), nil, "ls", "-l", "--nocolor", kbPath(d.name))
			if err != nil {
				return nil, err
			}
			d.entries = parseLs(out, time.Now())
			for _, e := range d.entries {
				d.fs.cache.put(path.Join(d.name, e.Name()), e.(*kbInfo))
			}
		}
		d.loaded = true
	}
	rest := d.entries[d.pos:]
	if count <= 0 {
		d.pos = len(d.entries)
		return rest, nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	if count > len(rest) {
		count = len(rest)
	}
	d.pos += count
	return rest[:count], nil
}

func contentType(name string) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// kbReader streams a file with `keybase fs read`. Sequential reads cost one
// process; a seek restarts the stream and skips forward, so random access is
// linear in the offset.
type kbReader struct {
	fs   *kbfsFS
	name string
	info *kbInfo
	ctx  context.Context

	off    int64 // logical position
	pos    int64 // position of the open stream
	cmd    *exec.Cmd
	stdout io.ReadCloser
	cancel context.CancelFunc
}

func (r *kbReader) Stat() (fs.FileInfo, error)                  { return r.info, nil }
func (r *kbReader) Readdir(int) ([]fs.FileInfo, error)          { return nil, errors.New("not a directory") }
func (r *kbReader) Write([]byte) (int, error)                   { return 0, fs.ErrPermission }
func (r *kbReader) ETag(context.Context) (string, error)        { return r.info.etag(), nil }
func (r *kbReader) ContentType(context.Context) (string, error) { return contentType(r.name), nil }

func (r *kbReader) Seek(offset int64, whence int) (int64, error) {
	var n int64
	switch whence {
	case io.SeekStart:
		n = offset
	case io.SeekCurrent:
		n = r.off + offset
	case io.SeekEnd:
		n = r.info.size + offset
	default:
		return 0, errors.New("bad whence")
	}
	if n < 0 {
		return 0, errors.New("negative offset")
	}
	r.off = n
	return n, nil
}

func (r *kbReader) start() error {
	if r.cmd != nil && r.pos == r.off {
		return nil
	}
	r.stop()
	if r.off >= r.info.size {
		return io.EOF
	}
	if err := r.fs.cli.acquire(r.ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, r.fs.cli.bin, "fs", "read", "-b", "1048576", kbPath(r.name))
	cmd.Env = r.fs.cli.env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		r.fs.cli.release()
		return err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		r.fs.cli.release()
		return err
	}
	r.cmd, r.stdout, r.cancel, r.pos = cmd, stdout, cancel, 0
	if r.off > 0 {
		if _, err := io.CopyN(io.Discard, stdout, r.off); err != nil {
			r.stop()
			return err
		}
		r.pos = r.off
	}
	return nil
}

func (r *kbReader) Read(p []byte) (int, error) {
	if err := r.start(); err != nil {
		return 0, err
	}
	n, err := r.stdout.Read(p)
	r.off += int64(n)
	r.pos += int64(n)
	return n, err
}

func (r *kbReader) stop() {
	if r.cmd == nil {
		return
	}
	r.cancel()
	_ = r.cmd.Wait()
	r.cmd, r.stdout, r.cancel = nil, nil, nil
	r.fs.cli.release()
}

func (r *kbReader) Close() error { r.stop(); return nil }

// kbWriter buffers writes in a temp file and uploads on Close with
// `keybase fs write`.
type kbWriter struct {
	ctx   context.Context
	fs    *kbfsFS
	name  string
	tmp   *os.File
	dirty bool
}

func newKBWriter(ctx context.Context, f *kbfsFS, name string, info fs.FileInfo, flag int) (*kbWriter, error) {
	tmp, err := os.CreateTemp("", "kbwebdav-*")
	if err != nil {
		return nil, err
	}
	w := &kbWriter{ctx: ctx, fs: f, name: name, tmp: tmp}
	if info == nil || flag&os.O_TRUNC != 0 {
		w.dirty = true // a new or truncated file must be created even if never written
		return w, nil
	}
	// Existing file opened for writing without truncation: seed the buffer.
	r := &kbReader{fs: f, name: name, info: info.(*kbInfo), ctx: ctx}
	defer r.Close()
	if _, err := io.Copy(tmp, readerOnly{r}); err != nil && err != io.EOF {
		w.discard()
		return nil, err
	}
	if flag&os.O_APPEND == 0 {
		_, _ = tmp.Seek(0, io.SeekStart)
	}
	return w, nil
}

type readerOnly struct{ io.Reader }

func (w *kbWriter) discard() {
	w.tmp.Close()
	os.Remove(w.tmp.Name())
}

func (w *kbWriter) Stat() (fs.FileInfo, error) {
	st, err := w.tmp.Stat()
	if err != nil {
		return nil, err
	}
	return &kbInfo{name: path.Base(w.name), size: st.Size(), mtime: time.Now()}, nil
}
func (w *kbWriter) Readdir(int) ([]fs.FileInfo, error)          { return nil, errors.New("not a directory") }
func (w *kbWriter) Read(p []byte) (int, error)                  { return w.tmp.Read(p) }
func (w *kbWriter) Seek(o int64, wh int) (int64, error)         { return w.tmp.Seek(o, wh) }
func (w *kbWriter) ContentType(context.Context) (string, error) { return contentType(w.name), nil }
func (w *kbWriter) ETag(context.Context) (string, error) {
	i, err := w.Stat()
	if err != nil {
		return "", err
	}
	return i.(*kbInfo).etag(), nil
}

func (w *kbWriter) Write(p []byte) (int, error) {
	w.dirty = true
	return w.tmp.Write(p)
}

func (w *kbWriter) Close() error {
	defer w.discard()
	if !w.dirty {
		return nil
	}
	if _, err := w.tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	// Do not tie the upload to the request context: the client has already
	// finished sending and the upload must complete.
	w.fs.cache.invalidate(w.name)
	_, err := w.fs.cli.run(context.Background(), w.tmp, "write", kbPath(w.name))
	return err
}
