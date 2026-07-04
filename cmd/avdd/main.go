// Command avdd is the avd-farm control plane: an HTTP API that launches,
// tracks, and reaps ephemeral Android emulator containers.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/avd-farm/avd-farm/internal/api"
	"github.com/avd-farm/avd-farm/internal/config"
	"github.com/avd-farm/avd-farm/internal/farm"
)

func main() {
	log.SetPrefix("avdd: ")
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	cfg, err := config.FromEnv()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	docker := farm.CLIDocker{}
	if err := docker.EnsureNetwork(ctx); err != nil {
		log.Fatalf("ensuring %s network: %v", farm.Network, err)
	}

	mgr := farm.NewManager(cfg, docker)
	if err := mgr.Reconcile(ctx); err != nil {
		log.Fatalf("reconciling existing devices: %v", err)
	}
	go mgr.RunReaper(ctx, 30*time.Second)

	srv := &http.Server{
		Addr:              cfg.Bind,
		Handler:           api.New(cfg, mgr).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("listening on %s (publish host %s, max %d devices)", cfg.Bind, cfg.PublishHost, cfg.MaxDevices)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("http server: %v", err)
	}
}
