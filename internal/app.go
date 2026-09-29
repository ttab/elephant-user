package internal

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephant-api/user/userconnect"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/pg"
)

type Parameters struct {
	Logger               *slog.Logger
	APIServer            *elephantine.APIServer
	AuthInfoParser       elephantine.AuthInfoParser
	Registerer           prometheus.Registerer
	Messages             *MessagesService
	Settings             *SettingsService
	ConfigurationService *ConfigurationService

	// Subscriber is the pg LISTEN subscriber to run alongside the
	// server. Optional; tests run their own.
	Subscriber *pg.Subscriber
	// Store and CleanupInterval configure the message retention cleaner,
	// which runs when both are set.
	Store           *PGStore
	CleanupInterval time.Duration
}

// subscriberRetryOptions restarts the LISTEN subscriber about every five
// seconds for as long as it keeps failing. Pinning the floor, the ceiling and
// the minimum runtime to the same value flattens the library's exponential
// curve into the static five second backoff the subscriber has always had,
// less the jitter on a run that outlived the minimum runtime: while it
// is down, long-polls only wake on their timeouts, so a restart that backed
// off towards the default one minute ceiling would stretch every outage by up
// to a minute for the price of one connection attempt per replica every five
// seconds. GiveUpAfter is left at zero, so it never gives up.
var subscriberRetryOptions = elephantine.RetryOptions{
	BackoffFloor: 5 * time.Second,
	BackoffCeil:  5 * time.Second,
	MinRuntime:   5 * time.Second,
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

	messagesServer := user.NewMessagesServer(p.Messages, opts.ServerOptions())
	settingsServer := user.NewSettingsServer(p.Settings, opts.ServerOptions())
	configurationServer := user.NewConfigurationServer(
		p.ConfigurationService, opts.ServerOptions())

	p.APIServer.RegisterAPI(messagesServer, opts)
	p.APIServer.RegisterAPI(settingsServer, opts)
	p.APIServer.RegisterAPI(configurationServer, opts)

	// The same services on the Connect paths (/elephant.user.<Service>/).
	handlerOpts := opts.HandlerOptions()

	messagesPath, messagesHandler := userconnect.NewMessagesServiceHandler(
		p.Messages, handlerOpts...)
	settingsPath, settingsHandler := userconnect.NewSettingsServiceHandler(
		p.Settings, handlerOpts...)
	configurationPath, configurationHandler := userconnect.NewConfigurationServiceHandler(
		p.ConfigurationService, handlerOpts...)

	p.APIServer.RegisterConnect(messagesPath, messagesHandler, opts)
	p.APIServer.RegisterConnect(settingsPath, settingsHandler, opts)
	p.APIServer.RegisterConnect(configurationPath, configurationHandler, opts)

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
		// failover resetting the LISTEN connection. Restart it forever
		// rather than taking the process down with it.
		grp.GoWithRetries("pubsub", subscriberRetryOptions,
			stopScoped(grace, func(ctx context.Context) error {
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
