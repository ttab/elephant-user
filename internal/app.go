package internal

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/pg"
)

type Parameters struct {
	Logger               *slog.Logger
	APIServer            *elephantine.APIServer
	AuthInfoParser       elephantine.AuthInfoParser
	Registerer           prometheus.Registerer
	Service              *Service
	ConfigurationService *ConfigurationService

	// Subscriber is the pg LISTEN subscriber to run alongside the
	// server. Optional; tests run their own.
	Subscriber *pg.Subscriber
	// Store and CleanupInterval configure the message retention cleaner,
	// which runs when both are set.
	Store           *PGStore
	CleanupInterval time.Duration
}

// Run serves the API and the background tasks until the context is
// cancelled or a task fails.
func Run(ctx context.Context, p Parameters) error {
	grace := elephantine.NewGracefulShutdown(p.Logger, 10*time.Second)

	opts, err := elephantine.NewDefaultServiceOptions(
		p.Logger, p.AuthInfoParser, p.Registerer,
		elephantine.ServiceAuthRequired,
	)
	if err != nil {
		return fmt.Errorf("set up service options: %w", err)
	}

	messagesServer := user.NewMessagesServer(p.Service, opts.ServerOptions())
	settingsServer := user.NewSettingsServer(p.Service, opts.ServerOptions())
	configurationServer := user.NewConfigurationServer(
		p.ConfigurationService, opts.ServerOptions())

	p.APIServer.RegisterAPI(messagesServer, opts)
	p.APIServer.RegisterAPI(settingsServer, opts)
	p.APIServer.RegisterAPI(configurationServer, opts)

	grp := elephantine.NewErrGroup(ctx, p.Logger,
		elephantine.WithErrGroupMetricsRegisterer(p.Registerer))

	// The server is the one task whose exit must stop everything: it
	// keeps serving in-flight requests until the quit deadline after
	// SIGTERM. The background tasks stop at once on SIGTERM instead, and
	// their clean exit must not cancel the group, or the server would be
	// shut down at stop time and the drain window lost.
	grp.Required("server", func(ctx context.Context) error {
		return p.APIServer.ListenAndServe(grace.CancelOnQuit(ctx))
	})

	if p.Subscriber != nil {
		// The subscriber reconnects by itself on ping timeouts but
		// returns on other connection errors, such as a database
		// failover resetting the LISTEN connection. Restart it with
		// backoff rather than taking the process down with it.
		grp.GoWithRetries("pubsub", 0, elephantine.StaticBackoff(5*time.Second),
			time.Hour, stopScoped(grace, func(ctx context.Context) error {
				return p.Subscriber.Run(ctx)
			}))
	}

	if p.Store != nil && p.CleanupInterval > 0 {
		grp.Go("cleaner", stopScoped(grace, func(ctx context.Context) error {
			return p.Store.RunCleaner(ctx, p.CleanupInterval, p.Registerer)
		}))
	}

	return grp.Wait() //nolint:wrapcheck
}

// stopScoped runs fn with a context that is cancelled when a graceful stop
// is requested, and treats a return caused by that stop as a clean exit
// rather than a task failure.
func stopScoped(
	grace *elephantine.GracefulShutdown,
	fn func(ctx context.Context) error,
) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		stopCtx := grace.CancelOnStop(ctx)

		err := fn(stopCtx)
		if err != nil && stopCtx.Err() == nil {
			return err
		}

		return nil
	}
}
