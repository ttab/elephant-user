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
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/pg"
	"github.com/urfave/cli/v3"
)

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
				Name:    "log-level",
				Sources: cli.EnvVars("LOG_LEVEL"),
				Value:   "debug",
			},
			&cli.StringFlag{
				Name:    "pg-conn-uri",
				Value:   "postgres://elephant-user:pass@localhost/elephant-user",
				Sources: cli.EnvVars("PG_CONN_URI", "CONN_STRING"),
			},
			&cli.StringSliceFlag{
				Name:    "cors-host",
				Usage:   "CORS hosts to allow, supports wildcards",
				Sources: cli.EnvVars("CORS_HOSTS"),
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
		addr         = cmd.String("addr")
		profileAddr  = cmd.String("profile-addr")
		logLevel     = cmd.String("log-level")
		pgConnString = cmd.String("pg-conn-uri")
		corsHosts    = cmd.StringSlice("cors-host")
	)

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

	dbpool, err := pgxpool.New(ctx, pgConnString)
	if err != nil {
		return fmt.Errorf("create connection pool: %w", err)
	}

	defer func() {
		// Don't block for close
		go dbpool.Close()
	}()

	err = dbpool.Ping(ctx)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}

	auth, err := elephantine.AuthenticationConfigFromCLI(ctx, cmd, nil)
	if err != nil {
		return fmt.Errorf("set up authentication: %w", err)
	}

	store := internal.NewPGStore(logger, dbpool)

	// Open a connection to the database and subscribes to all store notifications.
	go pg.Subscribe(
		ctx, logger, dbpool,
		store.Messages,
		store.InboxMessages,
	)

	go store.RunCleaner(ctx, 12*time.Hour)

	validator, err := internal.NewValidator(ctx)
	if err != nil {
		return fmt.Errorf("create validator: %w", err)
	}

	server := elephantine.NewAPIServer(logger, addr, profileAddr,
		elephantine.APIServerCORSHosts(corsHosts...))

	service := internal.NewService(logger, store, validator)

	err = internal.Run(ctx, internal.Parameters{
		Logger:         logger,
		APIServer:      server,
		AuthInfoParser: auth.AuthParser,
		Registerer:     prometheus.DefaultRegisterer,
		Service:        service,
	})
	if err != nil {
		return fmt.Errorf("run application: %w", err)
	}

	return nil
}
