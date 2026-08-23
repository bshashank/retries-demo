// Command server runs the pipeline-health simulation and serves its API and UI.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"pipelinehealth/internal/api"
	"pipelinehealth/web"
)

func main() {
	devFlag := flag.Bool("dev", false,
		"enable permissive CORS for the Vite dev server on http://localhost:5173 (never enable in production)")
	addrFlag := flag.String("addr", "", "listen address override; defaults to :$PORT")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	dev := *devFlag || truthy(os.Getenv("DEV"))

	addr := *addrFlag
	if addr == "" {
		// Cloud Run injects PORT and expects the container to honour it.
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		addr = ":" + port
	}

	ctrl, closeEngine := newSimController(logger)
	defer closeEngine()

	// baseCtx is the parent of every request context. Cancelling it on shutdown
	// tears down in-flight SSE streams, which otherwise never end on their own
	// and would make Server.Shutdown block until its deadline.
	baseCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()

	handler := api.NewRouter(ctrl, api.Options{
		Dev:    dev,
		Static: web.Dist(),
		Logger: logger,
	})

	srv := &http.Server{
		Addr:        addr,
		Handler:     handler,
		BaseContext: func(net.Listener) context.Context { return baseCtx },
		// ReadTimeout bounds reading the request line, headers, AND body from
		// connection accept. It's safe alongside a zero WriteTimeout because it
		// only ever governs the read side: a POST trickling its (already
		// 8KiB-capped) body in slowly gets cut off here instead of holding a
		// request slot open indefinitely, while GET /api/stream has no body to
		// read, so this never touches the SSE response that follows.
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout MUST stay zero. It is an absolute deadline on the whole
		// response, not an idle timeout, so any non-zero value silently severs
		// every SSE stream at that mark. ReadTimeout/ReadHeaderTimeout cover the
		// slowloris risk that WriteTimeout is usually reached for.
		WriteTimeout: 0,
		ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", addr, "dev", dev)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			logger.Error("server failed", "err", err)
			closeEngine()
			os.Exit(1)
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining")
		stop() // restore default signal handling: a second Ctrl-C kills immediately

		cancelRequests() // end SSE streams so Shutdown can finish promptly

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", "err", err)
		}
		logger.Info("stopped")
	}
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
