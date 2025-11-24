package main

import (
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
	"github.com/urfave/cli/v2"
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
				EnvVars: []string{"ADDR"},
				Value:   ":1080",
			},
			&cli.StringFlag{
				Name:    "profile-addr",
				EnvVars: []string{"PROFILE_ADDR"},
				Value:   ":1081",
			},
			&cli.StringFlag{
				Name:    "log-level",
				EnvVars: []string{"LOG_LEVEL"},
				Value:   "debug",
			},
			&cli.StringFlag{
				Name:    "pg-conn-uri",
				Value:   "postgres://elephant-user:pass@localhost/elephant-user",
				EnvVars: []string{"PG_CONN_URI", "CONN_STRING"},
			},
			&cli.StringSliceFlag{
				Name:    "cors-host",
				Usage:   "CORS hosts to allow, supports wildcards",
				EnvVars: []string{"CORS_HOSTS"},
			},
		},
	}

	runCmd.Flags = append(runCmd.Flags, elephantine.AuthenticationCLIFlags()...)

	app := cli.App{
		Name:  "user",
		Usage: "The Elephant user service",
		Commands: []*cli.Command{
			&runCmd,
		},
	}

	if err := app.Run(os.Args); err != nil {
		slog.Error("failed to run application",
			elephantine.LogKeyError, err)
		os.Exit(1)
	}
}

func runUser(c *cli.Context) error {
	var (
		addr         = c.String("addr")
		profileAddr  = c.String("profile-addr")
		logLevel     = c.String("log-level")
		pgConnString = c.String("pg-conn-uri")
		corsHosts    = c.StringSlice("cors-host")
	)

	logger := elephantine.SetUpLogger(logLevel, os.Stdout)

	defer func() {
		if p := recover(); p != nil {
			slog.ErrorContext(c.Context, "panic during setup",
				elephantine.LogKeyError, p,
				"stack", string(debug.Stack()),
			)

			os.Exit(2)
		}
	}()

	dbpool, err := pgxpool.New(c.Context, pgConnString)
	if err != nil {
		return fmt.Errorf("create connection pool: %w", err)
	}

	defer func() {
		// Don't block for close
		go dbpool.Close()
	}()

	err = dbpool.Ping(c.Context)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}

	auth, err := elephantine.AuthenticationConfigFromCLI(c, nil, nil)
	if err != nil {
		return fmt.Errorf("set up authentication: %w", err)
	}

	store := internal.NewPGStore(logger, dbpool)

	// Open a connection to the database and subscribes to all store notifications.
	go pg.Subscribe(
		c.Context, logger, dbpool,
		store.Messages,
		store.InboxMessages,
	)

	go store.RunCleaner(c.Context, 12*time.Hour)

	validator, err := internal.NewValidator(c.Context)
	if err != nil {
		return fmt.Errorf("create validator: %w", err)
	}

	server := elephantine.NewAPIServer(logger, addr, profileAddr,
		elephantine.APIServerCORSHosts(corsHosts...))

	service := internal.NewService(logger, store, validator)

	err = internal.Run(c.Context, internal.Parameters{
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
