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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephant-user/internal"
	"github.com/ttab/elephant-user/postgres"
	"github.com/ttab/elephant-user/schema"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/pg"
	"github.com/urfave/cli/v3"
)

var version string // set via -ldflags at build time

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
			&cli.BoolFlag{
				Name: "migrate-db",
				Usage: `Perform database migrations.
Intended for bootstrapping disposable environments. Having this always on in
production is a BAD IDEA! Migrations can be expensive and need to be planned.`,
				Sources: cli.EnvVars("MIGRATE_DB"),
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
		migrateDB         = cmd.Bool("migrate-db")
		cleanupInterval   = cmd.Duration("cleanup-interval")
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

	pubsubPool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return fmt.Errorf("create connection pool: %w", err)
	}

	defer func() {
		// Don't block for close
		go pubsubPool.Close()
	}()

	err = pubsubPool.Ping(ctx)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}

	dbpool := pubsubPool

	if bouncerConnString != "" && bouncerConnString != connString {
		dbpool, err = pgxpool.New(ctx, bouncerConnString)
		if err != nil {
			return fmt.Errorf("create bouncer connection pool: %w", err)
		}

		defer func() {
			go dbpool.Close()
		}()

		err = dbpool.Ping(ctx)
		if err != nil {
			return fmt.Errorf("connect to bouncer database: %w", err)
		}
	}

	// Pool metrics are registered where the pools are created; the
	// pubsub pool doubles as the main pool when no bouncer is configured.
	poolCollectors := map[string]*pgxpool.Pool{"main": dbpool}
	if dbpool != pubsubPool {
		poolCollectors["pubsub"] = pubsubPool
	}

	for name, pool := range poolCollectors {
		err = prometheus.DefaultRegisterer.Register(
			pg.NewPoolStatCollector(pool, name))
		if err != nil {
			return fmt.Errorf("register %s pool metrics: %w", name, err)
		}
	}

	if migrateDB {
		logger.Info("migrating database schema")

		// Migrate using the direct connection, tern doesn't play
		// well with transaction pooling.
		err = internal.Migrate(ctx, pubsubPool, schema.Migrations)
		if err != nil {
			return fmt.Errorf("migrate database: %w", err)
		}
	}

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

	server.Health.AddReadyFunction("postgres", dbpool.Ping)

	// Report schema state as part of readiness without failing the
	// probe: a freshly deployed service must be able to accept config
	// generation registrations through its own API.
	server.Health.AddOptionalReadyFunction("schemas",
		func(ctx context.Context) error {
			return schemasReadyCheck(ctx, store)
		})

	service := internal.NewService(logger, store, validator)
	configurationService := internal.NewConfigurationService(logger, store)

	err = internal.Run(ctx, internal.Parameters{
		Logger:               logger,
		APIServer:            server,
		AuthInfoParser:       auth.AuthParser,
		Registerer:           prometheus.DefaultRegisterer,
		Service:              service,
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
