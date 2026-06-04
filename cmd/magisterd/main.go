// Command magisterd is the orchestrator daemon: it owns the engine, the SQLite
// store, the supervisor, and the HTTP/SSE API. It resumes incomplete runs on
// startup and shuts down gracefully on SIGINT/SIGTERM.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"concentus/internal/api"
	"concentus/internal/config"
	"concentus/internal/core"
	"concentus/internal/engine"
	"concentus/internal/event"
	"concentus/internal/executor"
	"concentus/internal/gate"
	"concentus/internal/join"
	"concentus/internal/store"
	"concentus/internal/supervisor"
	"concentus/internal/workspace"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stopCh := make(chan struct{})
	go func() { <-ctx.Done(); close(stopCh) }()

	if err := run(os.Args[1:], os.Getenv, stopCh, nil); err != nil {
		slog.Error("magisterd exited with error", "err", err)
		os.Exit(1)
	}
}

// run is the testable daemon body. It serves until stopCh closes, then drains.
// onListen (optional) is called with the bound address once serving begins.
func run(args []string, env func(string) string, stopCh <-chan struct{}, onListen func(addr string)) error {
	cfg := config.Parse(args, env)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	reg := supervisor.NewApprovalRegistry()
	bus := event.NewBus()
	eng := &engine.Engine{
		Execs: map[string]core.Executor{"mock": executor.Mock{Name: "mock"}}, // real CLIAgents arrive in M4
		WS:    &workspace.GitManager{Root: filepath.Join(filepath.Dir(cfg.DBPath), "runs")},
		Gate:  &gate.Evaluator{Approver: &supervisor.RegistryApprover{Reg: reg}, Verifier: gate.CommandVerifier{}},
		Joins: join.Default(),
		Store: st, Bus: bus, Clock: core.SystemClock{}, Log: log,
	}
	sup := supervisor.New(eng, st, reg)
	sup.Log = log

	if err := sup.ResumeAll(context.Background()); err != nil {
		log.Error("resume incomplete runs", "err", err)
	}

	srv := &api.Server{Sup: sup, Store: st, Bus: bus, Log: log, BearerToken: cfg.BearerToken, ShutdownTimeout: cfg.ShutdownTimeout}
	httpSrv := &http.Server{
		Handler:      srv.Router(cfg.BearerToken),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // SSE streams are long-lived
		IdleTimeout:  60 * time.Second,
	}

	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	log.Info("listening", "addr", ln.Addr().String())
	if onListen != nil {
		go onListen(ln.Addr().String())
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case err := <-serveErr:
		return err
	case <-stopCh:
	}

	log.Info("shutting down")
	sup.Shutdown(cfg.ShutdownTimeout) // cancel active runs first
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}
