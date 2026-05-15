package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gnurub/bucketmux/internal/app"
	"github.com/gnurub/bucketmux/internal/config"
	"github.com/gnurub/bucketmux/internal/httpserver"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	configPath := os.Getenv("CONFIG_PATH")
	cfg, err := config.Load(configPath)
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(1)
	}
	ctx := context.Background()
	svc, err := app.NewService(ctx, cfg)
	if err != nil {
		logger.Error("create service", "error", err)
		os.Exit(1)
	}
	defer svc.Close()

	server := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           httpserver.NewHTTPHandler(svc),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("bucketmux listening", "addr", cfg.Server.Addr, "admin_enabled", cfg.Admin.Enabled)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown failed", "error", err)
		os.Exit(1)
	}
	logger.Info("shutdown complete")
}
