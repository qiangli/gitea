// Package embed provides a public API for embedding the Gitea server
// in external Go applications. It wraps the internal initialization
// and server lifecycle for use by the ycode agent harness.
//
// Gitea runs in-process — no external gitea binary or subprocess needed.
// The server is initialized programmatically using Gitea's Go libraries.
// If initialization fails (missing deps, DB errors, etc.), the server
// falls back to an error page instead of killing the host process.
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

	giteaLog "code.gitea.io/gitea/modules/log"
	"code.gitea.io/gitea/modules/setting"
	"code.gitea.io/gitea/routers"
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

// fatalError is the sentinel panic value used when Gitea calls log.Fatal.
type fatalError struct{ msg string }

// initGiteaSafe calls InitWebInstalled with log.Fatal redirected to panic,
// so the host process survives initialization failures.
func initGiteaSafe(ctx context.Context) (err error) {
	// Replace Gitea's os.Exit with panic so we can recover.
	origExiter := giteaLog.OsExiter
	giteaLog.OsExiter = func(code int) {
		panic(fatalError{msg: fmt.Sprintf("gitea init fatal (exit code %d)", code)})
	}
	defer func() {
		giteaLog.OsExiter = origExiter
		if r := recover(); r != nil {
			if fe, ok := r.(fatalError); ok {
				err = fmt.Errorf("%s", fe.msg)
			} else {
				err = fmt.Errorf("gitea init panic: %v", r)
			}
		}
	}()

	routers.InitWebInstalled(ctx)
	return nil
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

	var handler http.Handler

	// Initialize Gitea subsystems safely — log.Fatal panics instead of os.Exit.
	if err := initGiteaSafe(ctx); err != nil {
		// Initialization failed — serve an error page instead of crashing.
		errMsg := err.Error()
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `<!DOCTYPE html><html><head>
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s — Unavailable</title>
<style>body{font-family:system-ui;background:#0d1117;color:#e6edf3;padding:40px;max-width:600px;margin:0 auto}
h1{color:#f85149;margin-bottom:16px}pre{background:#161b22;border:1px solid #30363d;border-radius:8px;padding:16px;overflow-x:auto;font-size:13px;color:#ff7b72}</style>
</head><body><h1>Git Server Init Failed</h1>
<p>The embedded Gitea server failed to initialize. The git server tools and MCP endpoint are unavailable.</p>
<pre>%s</pre>
<p style="color:#8b949e;margin-top:24px">Check the ycode logs for details. Common causes: missing git binary, database errors, or permission issues.</p>
</body></html>`, setting.AppName, errMsg)
		})
		handler = mux
	} else {
		// Build the full Gitea web router (web UI + REST API + packages).
		handler = routers.NormalRoutes()
	}

	return &Server{
		handler: handler,
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
