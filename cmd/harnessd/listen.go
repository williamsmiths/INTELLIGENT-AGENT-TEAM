package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/williamsmiths/kongcowork-harness/internal/api"
)

// listenUnix opens a unix socket with mode 0600 in a directory with mode 0700
// (created when missing). harnessd has no TCP listener (AC-S2-006). A socket
// file left by a dead harnessd is removed; a live one, or anything that is
// not a socket, is an error and is left alone.
func listenUnix(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("socket directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("socket directory: %w", err)
	}
	switch st, err := os.Lstat(path); {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("socket %s: %w", path, err)
	case st.Mode()&os.ModeSocket == 0:
		return nil, fmt.Errorf("%s exists and is not a socket; refusing to replace it", path)
	default:
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("another harnessd is serving %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("stale socket %s: %w", path, err)
		}
	}
	// The umask makes the socket 0600 from the moment it exists; nothing else
	// runs yet, so changing it process-wide for one call is safe.
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("socket mode %s: %w", path, err)
	}
	return ln, nil
}

// serve runs one HTTP server per socket until ctx ends, then shuts both down.
func serve(ctx context.Context, handlers map[net.Listener]http.Handler) error {
	errc := make(chan error, len(handlers))
	var servers []*http.Server
	for ln, h := range handlers {
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ConnContext: api.ConnContext}
		servers = append(servers, srv)
		go func() { errc <- srv.Serve(ln) }()
	}
	var err error
	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range servers {
		if e := srv.Shutdown(shutdown); e != nil && err == nil {
			err = e
		}
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
