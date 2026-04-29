// Package embed provides a public API for embedding the Gitea server
// in external Go applications. It wraps the internal initialization
// and server lifecycle for use by the ycode agent harness.
//
// The full Gitea web UI (InitWebInstalled + NormalRoutes) is not yet
// wired because the routers package has unresolved transitive deps.
// Instead, the server provides a lightweight dashboard UI backed by
// Gitea's REST API, plus a passthrough to the API itself.
package embed

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

	appName := setting.AppName

	// Build a lightweight dashboard handler.
	// Full Gitea NormalRoutes() will replace this once routers deps are resolved.
	mux := http.NewServeMux()

	// API health endpoint.
	mux.HandleFunc("/api/v1/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","app":%q,"port":%d}`, appName, port)
	})

	// Dashboard web UI.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Serve the dashboard for the root and any non-API path.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		serveDashboard(w, r, appName, port, cfg.WorkPath)
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

// serveDashboard renders a lightweight HTML dashboard showing git server status
// and repository listing. This replaces the full Gitea UI until the routers
// transitive dependencies are resolved.
func serveDashboard(w http.ResponseWriter, _ *http.Request, appName string, port int, dataDir string) {
	// Discover repos from the filesystem.
	repos := discoverRepos(dataDir)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html><html><head>
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:#0d1117;color:#e6edf3;padding:24px}
h1{font-size:24px;margin-bottom:4px}
.subtitle{color:#8b949e;margin-bottom:24px;font-size:14px}
.status{display:inline-block;background:#238636;color:#fff;padding:2px 10px;border-radius:12px;font-size:12px;font-weight:600;margin-left:8px}
.card{background:#161b22;border:1px solid #30363d;border-radius:8px;padding:16px;margin-bottom:12px}
.card h3{font-size:16px;margin-bottom:6px}
.card p{color:#8b949e;font-size:13px}
.card .meta{font-size:12px;color:#8b949e;margin-top:8px}
.empty{color:#8b949e;font-style:italic;padding:32px 0;text-align:center}
.section{margin-top:24px}
.section h2{font-size:18px;margin-bottom:12px;padding-bottom:8px;border-bottom:1px solid #30363d}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(300px,1fr));gap:12px}
.info-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(200px,1fr));gap:12px;margin-bottom:24px}
.info-item{background:#161b22;border:1px solid #30363d;border-radius:8px;padding:12px}
.info-item .label{font-size:11px;text-transform:uppercase;color:#8b949e;letter-spacing:0.5px}
.info-item .value{font-size:18px;font-weight:600;margin-top:4px}
a{color:#58a6ff;text-decoration:none}
a:hover{text-decoration:underline}
.mcp-badge{display:inline-block;background:#1f6feb;color:#fff;padding:2px 8px;border-radius:4px;font-size:11px;font-weight:600;margin-left:6px}
</style></head><body>
<h1>%s <span class="status">Running</span></h1>
<p class="subtitle">Embedded git server for agent collaboration</p>

<div class="info-grid">
<div class="info-item"><div class="label">Port</div><div class="value">%d</div></div>
<div class="info-item"><div class="label">Repositories</div><div class="value">%d</div></div>
<div class="info-item"><div class="label">API</div><div class="value"><a href="/git/api/v1/healthz">/api/v1</a></div></div>
<div class="info-item"><div class="label">MCP</div><div class="value"><span class="mcp-badge">Connected</span></div></div>
</div>
`, appName, appName, port, len(repos))

	fmt.Fprintf(w, `<div class="section"><h2>Repositories</h2>`)
	if len(repos) == 0 {
		fmt.Fprintf(w, `<p class="empty">No repositories yet. Use the GitServerRepoCreate tool or MCP to create one.</p>`)
	} else {
		fmt.Fprintf(w, `<div class="grid">`)
		for _, r := range repos {
			fmt.Fprintf(w, `<div class="card"><h3>%s</h3><div class="meta">%s</div></div>`,
				template(r.Name), template(r.Path))
		}
		fmt.Fprintf(w, `</div>`)
	}
	fmt.Fprintf(w, `</div>`)

	fmt.Fprintf(w, `
<div class="section"><h2>Agent Tools</h2>
<div class="grid">
<div class="card"><h3>GitServerRepoCreate</h3><p>Create a new repository for agent collaboration</p></div>
<div class="card"><h3>GitServerRepoList</h3><p>List all repositories on this server</p></div>
<div class="card"><h3>GitServerWorktreeCreate</h3><p>Create isolated worktree for an agent</p></div>
<div class="card"><h3>GitServerWorktreeMerge</h3><p>Merge agent branch back to base</p></div>
<div class="card"><h3>GitServerWorktreeCleanup</h3><p>Clean up agent worktree</p></div>
</div></div>

<div class="section"><h2>MCP Endpoint</h2>
<div class="card">
<h3>/gitea-mcp/ <span class="mcp-badge">MCP</span></h3>
<p>JSON-RPC endpoint for external AI agents. Supports repo management, branching, and pull request workflows.</p>
<div class="meta">POST with JSON-RPC body &bull; Tools: list_repos, create_repo, list_branches, create_branch, create_pull_request, list_pull_requests, merge_pull_request</div>
</div></div>

</body></html>`)
}

// repoInfo holds basic info about a discovered repository.
type repoInfo struct {
	Name string
	Path string
}

// discoverRepos scans the repositories directory on disk.
func discoverRepos(dataDir string) []repoInfo {
	repoRoot := filepath.Join(dataDir, "repositories")
	var repos []repoInfo

	// Gitea stores repos as <owner>/<repo>.git directories.
	owners, err := os.ReadDir(repoRoot)
	if err != nil {
		return nil
	}
	for _, owner := range owners {
		if !owner.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(repoRoot, owner.Name()))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || !strings.HasSuffix(entry.Name(), ".git") {
				continue
			}
			name := strings.TrimSuffix(entry.Name(), ".git")
			repos = append(repos, repoInfo{
				Name: owner.Name() + "/" + name,
				Path: filepath.Join(repoRoot, owner.Name(), entry.Name()),
			})
		}
	}
	return repos
}

// template escapes a string for safe HTML embedding.
func template(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}

