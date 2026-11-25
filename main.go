// Command nepal-checkout is a small self-hosted payment server on top of
// github.com/voidarchive/nepal-payment-go: one merchant, one API key, amounts in paisa.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	a, err := newApp(cfg, time.Now)
	if err != nil {
		return err
	}
	defer func() { _ = a.store.Close() }()
	if cfg.Mock {
		slog.Warn("MOCK=true: the mock gateway is enabled; anyone can mark its payments paid. Never use it in production.")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           a.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second, // covers a slow gateway call in POST /v1/payments
		IdleTimeout:       120 * time.Second,
	}
	loopDone := make(chan struct{})
	go func() { a.loop(ctx); close(loopDone) }()

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("listening", "addr", cfg.ListenAddr, "public_url", cfg.PublicURL, "providers", slices.Sorted(maps.Keys(a.svc.Providers)))

	select {
	case err = <-errc:
		stop()
	case <-ctx.Done():
		slog.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err = srv.Shutdown(shutCtx)
	}
	<-loopDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
