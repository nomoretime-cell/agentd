package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"agentd/internal/agent"
)

func main() {
	if e := run(); e != nil {
		slog.Error("agentd stopped", "error", e)
		os.Exit(1)
	}
}
func run() error {
	config := flag.String("config", "config.yaml", "YAML configuration file")
	root := flag.String("root", ".", "project-local data and workspace root")
	flag.Parse()
	cfg, e := agent.LoadConfig(*config)
	if e != nil {
		return e
	}
	s, e := agent.Open(*root, cfg, nil)
	if e != nil {
		return e
	}
	defer s.Close()
	listener, e := net.Listen("tcp", cfg.Listen)
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	server := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	errorsCh := make(chan error, 1)
	go func() { errorsCh <- server.Serve(listener) }()
	s.Start(ctx)
	abs, _ := filepath.Abs(*root)
	slog.Info("agentd ready", "listen", listener.Addr().String(), "token_file", filepath.Join(abs, "data/api-token"), "database", filepath.Join(abs, "data/state.db"))
	select {
	case <-ctx.Done():
	case e = <-errorsCh:
		if e != http.ErrServerClosed {
			cancel()
			return fmt.Errorf("HTTP server: %w", e)
		}
	}
	shutdown, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	return server.Shutdown(shutdown)
}
