package main

import (
	"context"
	"errors"
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
	restart, e := run()
	if restart {
		if e != nil {
			slog.Warn("shutdown before restart was incomplete", "error", e)
		}
		e = reexec()
	}
	if e != nil {
		slog.Error("agentd stopped", "error", e)
		os.Exit(1)
	}
}

// reexec replaces this process with the same command after run has released
// the listener, the state lock and the database. The PID stays, so a terminal,
// systemd or a container keeps supervising it; other descriptors are
// close-on-exec.
func reexec() error {
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}

// checkLegacyLayout stops a default start next to state from before .agentd/
// existed. Moving data is left to the operator.
func checkLegacyLayout(explicitRoot bool) error {
	if explicitRoot {
		return nil
	}
	if _, e := os.Stat("data/state.db"); e != nil {
		return nil
	}
	if _, e := os.Stat(".agentd"); e == nil {
		return nil
	}
	return errors.New("found state from an older layout in ./data but no ./.agentd; move it with `mkdir .agentd && mv data workspace .agentd/`, or keep the old layout with -root .")
}

// run serves until a signal or an owner restart request; restart reports the latter.
func run() (bool, error) {
	config := flag.String("config", "config.yaml", "YAML configuration file")
	root := flag.String("root", ".agentd", "state root directory holding data/ and workspace/")
	flag.Parse()
	explicit := false
	flag.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "root" })
	if e := checkLegacyLayout(explicit); e != nil {
		return false, e
	}
	cfg, e := agent.LoadConfig(*config)
	if e != nil {
		return false, e
	}
	s, e := agent.Open(*root, cfg, nil)
	if e != nil {
		return false, e
	}
	defer s.Close()
	listener, e := net.Listen("tcp", cfg.Listen)
	if e != nil {
		return false, e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	server := &http.Server{Handler: withWorkbench(s.Handler()), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	errorsCh := make(chan error, 1)
	go func() { errorsCh <- server.Serve(listener) }()
	s.Start(ctx)
	abs, _ := filepath.Abs(*root)
	slog.Info("agentd ready", "listen", listener.Addr().String(), "token_file", filepath.Join(abs, "data/api-token"), "database", filepath.Join(abs, "data/state.db"))
	restart := false
	select {
	case <-ctx.Done():
	case <-s.Restarts():
		slog.Info("agentd restarting at the owner's request")
		restart = true
	case e = <-errorsCh:
		if e != http.ErrServerClosed {
			cancel()
			return false, fmt.Errorf("HTTP server: %w", e)
		}
	}
	shutdown, c := context.WithTimeout(context.Background(), 10*time.Second)
	defer c()
	return restart, server.Shutdown(shutdown)
}
