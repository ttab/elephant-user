package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"runtime/debug"

	"elephant-user/internal"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephantine"
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
				EnvVars: []string{"PG_CONN_URI"},
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

	service := internal.NewService(logger, nil)

	app, err := internal.NewApplication(c.Context, internal.ApplicationParameters{
		Addr:           addr,
		ProfileAddr:    profileAddr,
		Logger:         logger,
		DBPool:         dbpool,
		AuthInfoParser: auth.AuthParser,
		Registerer:     prometheus.DefaultRegisterer,
		Service:        service,
	})
	if err != nil {
		return fmt.Errorf("create application: %w", err)
	}

	err = app.Run(c.Context)
	if err != nil {
		return fmt.Errorf("run application: %w", err)
	}

	return nil
}
