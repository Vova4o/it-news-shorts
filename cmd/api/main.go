// Command api exposes health, readiness, and Prometheus metrics endpoints.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/it-news-shorts/it-news-shorts/internal/config"
	"github.com/it-news-shorts/it-news-shorts/internal/observability"
	"github.com/it-news-shorts/it-news-shorts/internal/postgres"
	"github.com/it-news-shorts/it-news-shorts/internal/storage"
	"github.com/it-news-shorts/it-news-shorts/internal/workflows"
)

func main() {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("load config failed", "error", err.Error())
		os.Exit(1)
	}

	logger := observability.NewLogger("api", cfg.App.LogLevel, os.Stdout)
	metrics := observability.NewMetrics()

	poolCfg, err := pgxpool.ParseConfig(cfg.Postgres.DSN)
	if err != nil {
		logger.Error("invalid postgres dsn", "error", err.Error())
		os.Exit(1)
	}
	poolCfg.MaxConns = cfg.Postgres.MaxConns
	poolCfg.MinConns = 0
	pgPool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		logger.Error("create postgres pool failed", "error", err.Error())
		os.Exit(1)
	}
	defer pgPool.Close()

	s3Store, err := storage.NewS3Store(context.Background(), cfg.MinIO)
	if err != nil {
		logger.Error("create s3 store failed", "error", err.Error())
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"status":  "ok",
			"service": "api",
		})
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		checks := runReadinessChecks(r.Context(), cfg, pgPool, s3Store, metrics, logger)
		allOK := true
		for _, v := range checks {
			if v != "ok" {
				allOK = false
				break
			}
		}
		statusCode := http.StatusOK
		overall := "ready"
		if !allOK {
			statusCode = http.StatusServiceUnavailable
			overall = "not_ready"
		}
		writeJSON(w, statusCode, map[string]any{
			"status": overall,
			"checks": checks,
		})
	})

	mux.Handle("GET /metrics", metrics.Handler())

	srv := &http.Server{
		Addr:         cfg.API.ListenAddr,
		Handler:      mux,
		ReadTimeout:  cfg.API.ReadTimeout,
		WriteTimeout: cfg.API.WriteTimeout,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api server listening", "addr", cfg.API.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		logger.Info("api server shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.API.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("api server shutdown error", "error", err.Error())
			os.Exit(1)
		}
	case err := <-errCh:
		if err != nil {
			logger.Error("api server failed", "error", err.Error())
			os.Exit(1)
		}
	}
}

func runReadinessChecks(
	ctx context.Context,
	cfg config.Config,
	pgPool *pgxpool.Pool,
	s3Store *storage.S3Store,
	metrics *observability.Metrics,
	logger *slog.Logger,
) map[string]string {
	const checkTimeout = 3 * time.Second
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		checks = make(map[string]string, 3)
	)

	record := func(name string, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			checks[name] = "unavailable"
			logger.Warn("readiness check failed", "dependency", name, "error", err.Error())
			return
		}
		checks[name] = "ok"
	}

	wg.Add(3)
	go func() {
		defer wg.Done()
		start := time.Now()
		err := postgres.Ping(ctx, pgPool, checkTimeout)
		metrics.ObserveDependency("postgres", "readyz_ping", time.Since(start), err)
		record("postgres", err)
	}()
	go func() {
		defer wg.Done()
		start := time.Now()
		c, cancel := context.WithTimeout(ctx, checkTimeout)
		defer cancel()
		err := s3Store.Ping(c)
		metrics.ObserveDependency("minio", "readyz_ping", time.Since(start), err)
		record("minio", err)
	}()
	go func() {
		defer wg.Done()
		start := time.Now()
		err := workflows.CheckTemporalReady(ctx, cfg.Temporal.Address, cfg.Temporal.Namespace, checkTimeout, logger)
		metrics.ObserveDependency("temporal", "readyz_ping", time.Since(start), err)
		record("temporal", err)
	}()

	wg.Wait()
	return checks
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
