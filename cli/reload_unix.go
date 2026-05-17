//go:build !windows

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mark3labs/mcp-go/server"
)

// watchReloadSignal listens for SIGHUP and triggers a tools reload on each one.
// On non-Windows platforms this allows operators to nudge the server without
// touching the tools file's mtime.
func watchReloadSignal(ctx context.Context, s *server.MCPServer, path string) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			n, err := reloadTools(s, path)
			if err != nil {
				slog.Error("reload (SIGHUP) failed", "err", err)
				continue
			}
			slog.Info("tools reloaded", "count", n, "trigger", "signal")
		}
	}
}
