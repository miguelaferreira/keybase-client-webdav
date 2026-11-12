// Command keybase-client-webdav serves KBFS over WebDAV on localhost so it can
// be mounted with macOS's built-in mount_webdav, with no FUSE kernel extension.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/net/webdav"
)

const defaultKeybase = "/Applications/Keybase.app/Contents/SharedSupport/bin/keybase"

func main() {
	home, _ := os.UserHomeDir()
	addr := flag.String("addr", "127.0.0.1:8484", "listen address (must be a loopback address)")
	bin := flag.String("keybase", defaultKeybase, "absolute path to the keybase binary")
	readOnly := flag.Bool("readonly", false, "reject all writes")
	tokenFile := flag.String("token-file", filepath.Join(home, ".config", "keybase-client-webdav", "token"), "file holding the secret URL prefix (created with mode 0600 if missing)")
	noAuth := flag.Bool("no-auth", false, "serve at / with no secret URL prefix (any local process can then read and write your KBFS)")
	procs := flag.Int("max-procs", 8, "maximum concurrent keybase processes")
	verbose := flag.Bool("verbose", false, "log every request with its status and duration")
	flag.Parse()

	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		log.Fatal(err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		log.Fatalf("refusing to listen on non-loopback address %q", *addr)
	}
	// The binary must be given by absolute path. Searching PATH is unsafe: PATH
	// may include directories that live on this very mount, and the lookup would
	// then call back into the server and deadlock it.
	if !filepath.IsAbs(*bin) {
		log.Fatalf("-keybase must be an absolute path, got %q", *bin)
	}
	if st, err := os.Stat(*bin); err != nil || st.IsDir() {
		log.Fatalf("keybase binary not found at %q", *bin)
	}

	prefix := ""
	if !*noAuth {
		token, err := loadToken(*tokenFile)
		if err != nil {
			log.Fatal(err)
		}
		prefix = "/" + token
	}

	var handler http.Handler = &webdav.Handler{
		Prefix:     prefix,
		FileSystem: newKBFS(newKeybaseCLI(*bin, *procs), *readOnly),
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				log.Printf("%s %s: %v", r.Method, strings.TrimPrefix(r.URL.Path, prefix), err)
			}
		},
	}
	if prefix != "" {
		handler = withPrefix(handler, prefix)
	}
	if *verbose {
		handler = withRequestLog(handler, prefix)
	}
	handler = withLocalHost(handler)

	srv := &http.Server{Addr: *addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("serving KBFS on http://%s (readonly=%v, secret-prefix=%v)", *addr, *readOnly, !*noAuth)
	log.Fatal(srv.ListenAndServe())
}

func loadToken(file string) (string, error) {
	if b, err := os.ReadFile(file); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(file, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// withPrefix answers only under the secret prefix. macOS's WebDAV client
// refuses HTTP Basic auth over plain HTTP, so the secret lives in the URL path
// instead (a capability URL). The comparison is constant-time.
func withPrefix(next http.Handler, prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if len(p) < len(prefix) || subtle.ConstantTimeCompare([]byte(p[:len(prefix)]), []byte(prefix)) != 1 ||
			(len(p) > len(prefix) && p[len(prefix)] != '/') {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withLocalHost rejects requests whose Host header is not a loopback name, to
// block DNS-rebinding attacks from web pages.
func withLocalHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			h = r.Host
		}
		if ip := net.ParseIP(strings.Trim(h, "[]")); h != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, fmt.Sprintf("bad host %q", h), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// withRequestLog logs each request when it starts and when it finishes, so a
// request that never finishes is visible. The secret prefix is not logged.
func withRequestLog(next http.Handler, prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, prefix)
		start := time.Now()
		log.Printf("-> %s %s depth=%q", r.Method, p, r.Header.Get("Depth"))
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("<- %s %s %d %s", r.Method, p, rec.status, time.Since(start).Round(time.Millisecond))
	})
}
