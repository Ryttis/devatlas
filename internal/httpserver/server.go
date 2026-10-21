// Package httpserver implements the DevAtlas local HTTP server: a loopback-only
// listener serving a JSON health endpoint and a server-rendered home page.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/Ryttis/devatlas/internal/version"
)

// Server is a configured, not yet listening, DevAtlas HTTP server.
type Server struct {
	cfg     Config
	log     *slog.Logger
	http    *http.Server
	started time.Time
}

// New builds a server from cfg. It returns an error if cfg is invalid; no
// socket is opened until Listen is called.
func New(cfg Config, log *slog.Logger) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	s := &Server{cfg: cfg, log: log}
	s.http = &http.Server{
		Handler: s.routes(),
		// Timeouts protect the process from slow or stalled clients. The
		// values are generous for a local tool and can be tuned later.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return s, nil
}

// Listen opens the TCP listener on the configured loopback address. A port
// that is already occupied is reported as an *AddrInUseError so callers can
// explain the conflict rather than print a raw socket error.
func (s *Server) Listen() (net.Listener, error) {
	addr := s.cfg.Addr()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, &AddrInUseError{Addr: addr, Err: err}
		}
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return ln, nil
}

// Serve accepts connections on ln until ctx is cancelled, then performs a
// graceful shutdown bounded by Config.ShutdownTimeout. It returns nil after a
// clean shutdown and an error if serving or shutting down failed.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.started = time.Now()
	s.log.Info("devatlas listening", "url", "http://"+ln.Addr().String(), "version", version.Current().Version)

	serveErr := make(chan error, 1)
	go func() {
		err := s.http.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	s.log.Info("shutting down", "timeout", s.cfg.ShutdownTimeout)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		// Shutdown already closed the listener; also close lingering
		// connections so the process can exit promptly.
		_ = s.http.Close()
		return fmt.Errorf("graceful shutdown did not complete: %w", err)
	}
	if err := <-serveErr; err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	s.log.Info("shutdown complete")
	return nil
}

// AddrInUseError reports that the requested address is occupied by another
// process. DevAtlas does not yet identify that process (port-conflict
// analysis is planned for a later slice).
type AddrInUseError struct {
	Addr string
	Err  error
}

func (e *AddrInUseError) Error() string {
	return fmt.Sprintf("address %s is already in use by another process; choose a different port with -port", e.Addr)
}

// Unwrap exposes the underlying socket error.
func (e *AddrInUseError) Unwrap() error { return e.Err }
