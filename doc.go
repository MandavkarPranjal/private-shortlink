// Package shortlink is a private, golink-style URL shortener that runs on a
// Tailscale tailnet (via tsnet), on a control-plane-free tailcat address, or
// as a plain local server.
//
// The executable lives in cmd/shortlink; the library packages are
// internal/store (SQLite persistence) and internal/web (HTTP UI and JSON API).
package shortlink
