package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"runtime/debug"
	"time"

	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephant-user/internal"
	"github.com/ttab/elephant-user/postgres"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/pg"
	"github.com/urfave/cli/v3"
)

var version string // set via -ldflags at build time

// defaultDBMaxConns is the size of the query pool, set here rather than left
// to pgx: its default is max(4, NumCPU()) read from the node's cpuset rather
// than the cgroup quota, so an unset pool tracks whichever node the pod lands
// on and changes size invisibly on reschedule.
//
// Every RPC runs one to three short queries and holds no connection while a
// long-poll waits, so the pool is sized for a burst of concurrent writes plus
// the cleaner's lock ping and sweep and the validator's reload. Sixteen leaves
// room for that without approaching a bouncer's per-client limit. Trim it once
// pgxpool_empty_acquire_wait_seconds_total says what it actually needs.
const defaultDBMaxConns = 16

func main() {
	err := godotenv.Load()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Error("exiting: ",
			elephantine.LogKeyError, err)
		os.Exit(1)
	}

	runCmd := cli.Command{
		Name:        "run",
		Description: "Runs the service",
		Action:      runUser,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "addr",
				Sources: cli.EnvVars("ADDR"),
				Value:   ":1080",
			},
			&cli.StringFlag{
				Name:    "profile-addr",
				Sources: cli.EnvVars("PROFILE_ADDR"),
				Value:   ":1081",
			},
			&cli.StringFlag{
				Name:    "tls-addr",
				Value:   ":1443",
				Sources: cli.EnvVars("TLS_ADDR", "TLS_LISTEN_ADDR"),
			},
			&cli.StringFlag{
				Name:    "cert-file",
				Sources: cli.EnvVars("TLS_CERT_PATH"),
			},
			&cli.StringFlag{
				Name:    "key-file",
				Sources: cli.EnvVars("TLS_KEY_PATH"),
			},
			&cli.StringFlag{
				Name:    "log-level",
				Sources: cli.EnvVars("LOG_LEVEL"),
				Value:   "debug",
			},
			&cli.StringFlag{ //nolint:gosec // G101: Default dev connection string, not real credentials.
				Name:    "db",
				Value:   "postgres://elephant-user:pass@localhost/elephant-user",
				Sources: cli.EnvVars("CONN_STRING"),
			},
			&cli.StringFlag{
				Name:    "db-bouncer",
				Sources: cli.EnvVars("BOUNCER_CONN_STRING"),
			},
			&cli.IntFlag{
				Name:    "db-max-conns",
				Sources: cli.EnvVars("DB_MAX_CONNS"),
				Value:   defaultDBMaxConns,
				Usage: `Maximum size of the Postgres connection pool used for
queries. Overrides pool_max_conns in the connection string. Zero or less leaves
the pool to size itself, which means max(4, NumCPU()) read from the node's
cpuset. With a bouncer configured the direct pool is fixed at 2 and this applies
to the bouncer pool.`,
			},
			&cli.StringSliceFlag{
				Name:    "cors-host",
				Usage:   "CORS hosts to allow, supports wildcards",
				Sources: cli.EnvVars("CORS_HOSTS"),
			},
			&cli.DurationFlag{
				Name: "cleanup-interval",
				Usage: `How often expired messages and inbox messages are
removed. Runs on one replica at a time under a job lock.`,
				Sources: cli.EnvVars("CLEANUP_INTERVAL"),
				Value:   12 * time.Hour,
			},
		},
	}

	runCmd.Flags = append(runCmd.Flags, elephantine.AuthenticationCLIFlags()...)

	app := cli.Command{
		Name:  "user",
		Usage: "The Elephant user service",
		Commands: []*cli.Command{
			&runCmd,
		},
	}

	if err := app.Run(context.Background(), os.Args); err != nil {
		slog.Error("failed to run application",
			elephantine.LogKeyError, err)
		os.Exit(1)
	}
}

func runUser(ctx context.Context, cmd *cli.Command) error {
	var (
		addr              = cmd.String("addr")
		profileAddr       = cmd.String("profile-addr")
		tlsAddr           = cmd.String("tls-addr")
		certFile          = cmd.String("cert-file")
		keyFile           = cmd.String("key-file")
		logLevel          = cmd.String("log-level")
		connString        = cmd.String("db")
		bouncerConnString = cmd.String("db-bouncer")
		corsHosts         = cmd.StringSlice("cors-host")
		cleanupInterval   = cmd.Duration("cleanup-interval")
		dbMaxConns        = cmd.Int("db-max-conns")
	)

	if cleanupInterval <= 0 {
		return fmt.Errorf("cleanup-interval must be positive, got %s", cleanupInterval)
	}

	logger := elephantine.SetUpLogger(logLevel, os.Stdout)

	defer func() {
		if p := recover(); p != nil {
			slog.ErrorContext(ctx, "panic during setup",
				elephantine.LogKeyError, p,
				"stack", string(debug.Stack()),
			)

			os.Exit(2)
		}
	}()

	// LISTEN is session-level and doesn't survive transaction pooling, so
	// behind a bouncer the subscriber keeps a direct pool of
	// pg.DefaultPubSubMaxConns while DB_MAX_CONNS sizes the bouncer pool
	// the queries run on. Without a bouncer, or with one equal to
	// CONN_STRING, the direct pool is the only pool. The pools register
	// their metrics as "main" and, when separate, "pubsub".
	pools, err := pg.NewPools(ctx, prometheus.DefaultRegisterer,
		connString, dbMaxConns,
		pg.WithBouncer(bouncerConnString),
		pg.WithPubSub(),
	)
	if err != nil {
		return fmt.Errorf("create database pools: %w", err)
	}

	defer func() {
		// Don't block for close
		go pools.Close()
	}()

	dbpool, pubsubPool := pools.Main, pools.PubSub

	logger.InfoContext(ctx, "created connection pools",
		"max_conns", dbpool.Config().MaxConns,
		"direct_max_conns", pubsubPool.Config().MaxConns,
		"bouncer", dbpool != pubsubPool)

	auth, err := elephantine.AuthenticationConfigFromCLI(ctx, cmd, nil)
	if err != nil {
		return fmt.Errorf("set up authentication: %w", err)
	}

	metrics, err := internal.NewMetrics(prometheus.DefaultRegisterer)
	if err != nil {
		return fmt.Errorf("set up metrics: %w", err)
	}

	store := internal.NewPGStore(logger, dbpool)

	validator, err := internal.NewValidator(ctx, logger, store, metrics)
	if err != nil {
		return fmt.Errorf("create validator: %w", err)
	}

	// LISTEN on the direct pool: session-level LISTEN is incompatible
	// with transaction pooling. Notifications missed while the connection
	// is down are caught up by the validator's periodic recheck.
	subscriber := store.NewSubscriber(pubsubPool)

	serverOpts := []elephantine.APIServerOption{
		elephantine.APIServerCORSHosts(corsHosts...),
		elephantine.APIServerVersion(version),
	}

	if certFile != "" {
		serverOpts = append(serverOpts,
			elephantine.APIServerTLS(tlsAddr, certFile, keyFile))
	}

	server := elephantine.NewAPIServer(logger, addr, profileAddr, serverOpts...)

	// Report database reachability without gating readiness on it: a
	// starved pool would otherwise fail the probe on every replica at once
	// and take the whole service out of the load balancer while it is still
	// serving.
	server.Health.AddOptionalReadyFunction("postgres", dbpool.Ping)

	// Report schema state as part of readiness without failing the
	// probe: a freshly deployed service must be able to accept config
	// generation registrations through its own API.
	server.Health.AddOptionalReadyFunction("schemas",
		func(ctx context.Context) error {
			return schemasReadyCheck(ctx, store)
		})

	messagesService := internal.NewMessagesService(logger, store, validator)
	settingsService := internal.NewSettingsService(logger, store, validator)
	configurationService := internal.NewConfigurationService(logger, store)

	err = internal.Run(ctx, internal.Parameters{
		Logger:               logger,
		APIServer:            server,
		AuthInfoParser:       auth.AuthParser,
		Registerer:           prometheus.DefaultRegisterer,
		Messages:             messagesService,
		Settings:             settingsService,
		ConfigurationService: configurationService,
		Subscriber:           subscriber,
		Store:                store,
		CleanupInterval:      cleanupInterval,
	})
	if err != nil {
		return fmt.Errorf("run application: %w", err)
	}

	return nil
}

// schemasReadyCheck reports whether the active config generation has
// schemas for both settings and messages.
func schemasReadyCheck(ctx context.Context, store *internal.PGStore) error {
	schemas, err := store.GetActiveSchemas(ctx)
	if err != nil {
		return fmt.Errorf("get active generation schemas: %w", err)
	}

	found := make(map[postgres.SchemaUsage]bool)

	for _, schema := range schemas {
		found[schema.Usage] = true
	}

	for _, usage := range []postgres.SchemaUsage{
		postgres.SchemaUsageSettings,
		postgres.SchemaUsageMessages,
	} {
		if !found[usage] {
			return fmt.Errorf(
				"no active schema for usage %q", usage)
		}
	}

	return nil
}
