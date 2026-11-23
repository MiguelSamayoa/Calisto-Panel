package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/MiguelSamayoa/calisto-panel/internal/actions"
	"github.com/MiguelSamayoa/calisto-panel/internal/config"
	"github.com/MiguelSamayoa/calisto-panel/internal/httpapi"
	"github.com/MiguelSamayoa/calisto-panel/internal/hub"
	"github.com/MiguelSamayoa/calisto-panel/internal/lhm"
	"github.com/MiguelSamayoa/calisto-panel/internal/winutil"
	"github.com/MiguelSamayoa/calisto-panel/web"
)

const shutdownGrace = 5 * time.Second

func main() {
	configPath := flag.String("config", `C:\ProgramData\CalistoPanel\config.yaml`, "path to config.yaml")
	flag.Parse()
	if err := run(*configPath); err != nil {
		fmt.Fprintln(os.Stderr, "panel:", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if cfg.CIEnabled() {
		return errors.New("ci pipelines are configured but the azure devops provider is not implemented yet")
	}

	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.Log.Level)) // validated by config.Load
	log := slog.New(slog.NewJSONHandler(&lumberjack.Logger{
		Filename:   cfg.Log.File,
		MaxSize:    cfg.Log.MaxSizeMB,
		MaxBackups: cfg.Log.MaxBackups,
	}, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var h *hub.Hub // set below; the executor only reports after the server starts
	exec := actions.New(ctx, cfg.Actions, actions.Deps{
		Run:    winutil.Run,
		Keys:   winutil.SendMediaKey,
		Report: func(u hub.ActionUpdate) { h.UpdateAction(u) },
		Log:    log,
	})
	if h, err = hub.New(cfg, exec.IDs()); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:    cfg.Server.Listen,
		Handler: httpapi.New(cfg.Secrets.APIToken.Reveal(), web.Index, h.ServeStream, exec.Do, log),
		// No WriteTimeout: it would cut the long-lived SSE stream.
		ReadHeaderTimeout: 5 * time.Second,
	}

	var pollers sync.WaitGroup
	pollers.Add(1)
	go func() {
		defer pollers.Done()
		client := lhm.NewClient(cfg.Sensors.URL, time.Duration(cfg.Sensors.TimeoutMs)*time.Millisecond)
		pollSensors(ctx, cfg.Sensors, client, h, log)
	}()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	log.Info("panel started", "listen", cfg.Server.Listen, "actions", exec.IDs(), "ci", cfg.CIEnabled())

	select {
	case err = <-serveErr:
		stop()
	case <-ctx.Done():
		err = nil
	}

	h.Close() // ends open SSE streams so Shutdown does not wait on them
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if serr := srv.Shutdown(shutCtx); serr != nil && err == nil {
		err = serr
	}
	exec.Wait()
	pollers.Wait()
	log.Info("panel stopped")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
