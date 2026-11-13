package workflows

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	temporallog "go.temporal.io/sdk/log"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/it-news-shorts/it-news-shorts/internal/config"
)

func temporalLogger(logger *slog.Logger) temporallog.Logger {
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	return temporallog.NewStructuredLogger(logger)
}

// DialTemporal connects to the Temporal Server for the configured namespace,
// honoring ctx cancellation and deadline.
func DialTemporal(ctx context.Context, cfg config.TemporalConfig, logger *slog.Logger) (client.Client, error) {
	opts := client.Options{
		HostPort:  cfg.Address,
		Namespace: cfg.Namespace,
		Logger:    temporalLogger(logger),
	}
	c, err := client.DialContext(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("temporal: dial %s (namespace %s): %w", cfg.Address, cfg.Namespace, err)
	}
	return c, nil
}

// EnsureNamespace idempotently registers the Temporal namespace if it does not
// exist yet.
func EnsureNamespace(ctx context.Context, address, namespace string, retention time.Duration, logger *slog.Logger) error {
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	nsClient, err := client.NewNamespaceClient(client.Options{
		HostPort: address,
		Logger:   temporalLogger(logger),
	})
	if err != nil {
		return fmt.Errorf("temporal: create namespace client: %w", err)
	}
	defer nsClient.Close()

	opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, err = nsClient.Describe(opCtx, namespace)
	if err == nil {
		return nil
	}
	var notFound *serviceerror.NamespaceNotFound
	if !errors.As(err, &notFound) {
		return fmt.Errorf("temporal: describe namespace %q: %w", namespace, err)
	}

	err = nsClient.Register(opCtx, &workflowservice.RegisterNamespaceRequest{
		Namespace:                        namespace,
		WorkflowExecutionRetentionPeriod: durationpb.New(retention),
		Description:                      "it-news-shorts production workflows",
	})
	var alreadyExists *serviceerror.NamespaceAlreadyExists
	if err != nil && !errors.As(err, &alreadyExists) {
		return fmt.Errorf("temporal: register namespace %q: %w", namespace, err)
	}
	return nil
}

// CheckTemporalReady verifies Temporal Server connectivity and namespace
// existence with a bounded timeout.
func CheckTemporalReady(ctx context.Context, address, namespace string, timeout time.Duration, logger *slog.Logger) error {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	nsClient, err := client.NewNamespaceClient(client.Options{
		HostPort: address,
		Logger:   temporalLogger(logger),
	})
	if err != nil {
		return fmt.Errorf("temporal: create namespace client: %w", err)
	}
	defer nsClient.Close()

	if _, err := nsClient.Describe(opCtx, namespace); err != nil {
		return fmt.Errorf("temporal: describe namespace %q: %w", namespace, err)
	}
	return nil
}
