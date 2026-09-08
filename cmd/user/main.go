package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
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

// listenPoolMaxConns is the size of the direct pool when queries go through a
// bouncer: it then carries only the LISTEN session, which the subscriber
// hijacks out of the pool, and the startup migration.
const listenPoolMaxConns = 2

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

	useBouncer := bouncerConnString != "" && bouncerConnString != connString

	pubsubMaxConns := dbMaxConns
	if useBouncer {
		pubsubMaxConns = listenPoolMaxConns
	}

	pubsubPool, err := newPool(ctx, connString, pubsubMaxConns)
	if err != nil {
		return fmt.Errorf("direct database: %w", err)
	}

	defer func() {
		// Don't block for close
		go pubsubPool.Close()
	}()

	dbpool := pubsubPool

	if useBouncer {
		dbpool, err = newPool(ctx, bouncerConnString, dbMaxConns)
		if err != nil {
			return fmt.Errorf("bouncer database: %w", err)
		}

		defer func() {
			go dbpool.Close()
		}()
	}

	logger.InfoContext(ctx, "created connection pools",
		"max_conns", dbMaxConns,
		"direct_max_conns", pubsubMaxConns,
		"bouncer", useBouncer)

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

// newPool creates a connection pool and verifies that the database answers.
// A positive maxConns sizes the pool; zero or less leaves that to the
// connection string or pgx.
func newPool(
	ctx context.Context, connString string, maxConns int,
) (*pgxpool.Pool, error) {
	conf, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}

	if maxConns > math.MaxInt32 {
		return nil, fmt.Errorf("max conns %d exceeds %d", maxConns, math.MaxInt32)
	}

	if maxConns > 0 {
		conf.MaxConns = int32(maxConns)
	}

	pool, err := pgxpool.NewWithConfig(ctx, conf)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	err = pool.Ping(ctx)
	if err != nil {
		pool.Close()

		return nil, fmt.Errorf("connect to database: %w", err)
	}

	return pool, nil
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
