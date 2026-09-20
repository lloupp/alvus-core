package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lloupp/alvus-core/internal/config"
	"github.com/lloupp/alvus-core/internal/gateway"
)

func main() {
	configPath := flag.String("config", os.Getenv("ALVUS_CONFIG"), "path to JSON configuration")
	watch := flag.Bool("watch", true, "hot reload configuration when the file changes")
	flag.Parse()
	if *configPath == "" {
		*configPath = "alvus.json"
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("configuration error", "error", err)
		os.Exit(1)
	}

	gw := gateway.New(cfg, logger)
	server := &http.Server{
		Addr:              cfg.Listen,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	stopWatch := make(chan struct{})
	if *watch {
		go watchConfig(*configPath, gw, logger, stopWatch)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stop
		close(stopWatch)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	logger.Info("alvus-core started", "listen", cfg.Listen, "providers", len(cfg.Providers), "models", len(cfg.Models), "watch", *watch)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func watchConfig(path string, gw *gateway.Server, logger *slog.Logger, stop <-chan struct{}) {
	var lastMod time.Time
	if info, err := os.Stat(path); err == nil {
		lastMod = info.ModTime()
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			info, err := os.Stat(path)
			if err != nil || !info.ModTime().After(lastMod) {
				continue
			}
			lastMod = info.ModTime()
			cfg, err := config.Load(path)
			if err != nil {
				logger.Error("configuration reload rejected; keeping previous state", "error", err)
				continue
			}
			if err := gw.Reload(cfg); err != nil {
				logger.Error("configuration reload rejected; keeping previous state", "error", err)
			}
		}
	}
}
