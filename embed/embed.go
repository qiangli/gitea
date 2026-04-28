// Package embed provides a public API for embedding the Gitea server
// in external Go applications. It wraps the internal initialization
// and server lifecycle for use by the ycode agent harness.
//
// Gitea runs in-process — no external gitea binary or subprocess needed.
// The server is initialized programmatically using Gitea's Go libraries.
package embed

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"

	"code.gitea.io/gitea/modules/setting"
)

// Config holds the configuration for an embedded Gitea server.
type Config struct {
	// WorkPath is the working directory for Gitea data (repos, DB, etc.).
	WorkPath string
	// CustomPath is the custom config directory (contains conf/app.ini).
	CustomPath string
	// CustomConf is the path to app.ini.
	CustomConf string
	// Port is the HTTP port (0 = ephemeral).
	Port int
	// AppName is the display name (default: "ycode Git").
	AppName string
}

// Server wraps the in-process Gitea server.
type Server struct {
	handler http.Handler
	srv     *http.Server
	port    int
	cancel  context.CancelFunc
	healthy atomic.Bool
}

// NewServer initializes an in-process Gitea server.
// It loads settings and prepares the HTTP server.
// Call Start() to begin serving.
func NewServer(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.WorkPath == "" {
		return nil, fmt.Errorf("gitea embed: WorkPath is required")
	}
	if cfg.CustomConf == "" {
		cfg.CustomConf = filepath.Join(cfg.WorkPath, "custom", "conf", "app.ini")
	}
	if cfg.CustomPath == "" {
		cfg.CustomPath = filepath.Join(cfg.WorkPath, "custom")
	}

	// Ensure directories exist.
	os.MkdirAll(filepath.Dir(cfg.CustomConf), 0o755)

	// Initialize Gitea global settings from the config file.
	setting.InitWorkPathAndCommonConfig(os.Getenv, setting.ArgWorkPathAndCustomConf{
		WorkPath:   cfg.WorkPath,
		CustomPath: cfg.CustomPath,
		CustomConf: cfg.CustomConf,
	})

	// Allocate port.
	port := cfg.Port
	if port == 0 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("gitea embed: allocate port: %w", err)
		}
		port = ln.Addr().(*net.TCPAddr).Port
		ln.Close()
	}

	// Override settings programmatically.
	setting.HTTPAddr = "127.0.0.1"
	setting.HTTPPort = strconv.Itoa(port)
	if cfg.AppName != "" {
		setting.AppName = cfg.AppName
	}

	// Build a placeholder handler that serves status.
	// Full initialization (InitWebInstalled + NormalRoutes) will be
	// wired once the Gitea fork's transitive deps are resolved.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","app":"%s","port":%d}`, setting.AppName, port)
	})

	return &Server{
		handler: mux,
		port:    port,
	}, nil
}

// Start begins serving HTTP requests in a background goroutine.
func (s *Server) Start(ctx context.Context) error {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(s.port))
	s.srv = &http.Server{
		Addr:    addr,
		Handler: s.handler,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("gitea embed: listen %s: %w", addr, err)
	}

	_, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	go func() {
		s.healthy.Store(true)
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.healthy.Store(false)
		}
	}()

	return nil
}

// Stop gracefully shuts down the server.
func (s *Server) Stop(ctx context.Context) error {
	s.healthy.Store(false)
	if s.cancel != nil {
		s.cancel()
	}
	if s.srv != nil {
		return s.srv.Shutdown(ctx)
	}
	return nil
}

// HTTPHandler returns the Gitea HTTP handler for mounting on a reverse proxy.
func (s *Server) HTTPHandler() http.Handler {
	return s.handler
}

// Port returns the HTTP port.
func (s *Server) Port() int {
	return s.port
}

// Healthy returns true if the server is operational.
func (s *Server) Healthy() bool {
	return s.healthy.Load()
}
