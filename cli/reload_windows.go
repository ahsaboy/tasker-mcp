//go:build windows

package main

import (
	"context"

	"github.com/mark3labs/mcp-go/server"
)

// watchReloadSignal is a no-op on Windows: syscall.SIGHUP is not delivered
// reliably (or at all) by the Go runtime there, so we rely solely on the
// mtime-based watcher to trigger reloads.
func watchReloadSignal(ctx context.Context, s *server.MCPServer, path string) {
	<-ctx.Done()
}
