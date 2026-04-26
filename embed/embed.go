// Package embed provides a public API for embedding the Gitea server
// in external Go applications. It wraps the internal initialization
// and server lifecycle for use by the ycode agent harness.
//
// The Gitea server provides git repository management, branch isolation,
// PR workflows, and a web UI for agent swarm coordination.
package embed

import (
	"code.gitea.io/gitea/modules/setting"
)

// Config holds the configuration for an embedded Gitea server.
type Config struct {
	// CustomPath is the custom config directory (contains conf/app.ini).
	CustomPath string
	// WorkPath is the working directory for Gitea data.
	WorkPath string
	// CustomConf is the path to app.ini.
	CustomConf string
	// Port for the HTTP server.
	Port int
	// AppName is the display name.
	AppName string
}

// InitSettings initializes Gitea's global settings from the given config.
// This must be called before starting the server.
func InitSettings(cfg Config) {
	if cfg.CustomPath != "" {
		setting.CustomPath = cfg.CustomPath
	}
	if cfg.WorkPath != "" {
		setting.AppWorkPath = cfg.WorkPath
	}
	if cfg.CustomConf != "" {
		setting.CustomConf = cfg.CustomConf
	}
}
