package internal

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephantine"
)

type ApplicationParameters struct {
	Addr           string
	ProfileAddr    string
	Logger         *slog.Logger
	DBPool         *pgxpool.Pool
	AuthInfoParser elephantine.AuthInfoParser
	Registerer     prometheus.Registerer
	Service        *Service
}

func NewApplication(
	_ context.Context, p ApplicationParameters,
) (*Application, error) {
	return &Application{
		p: p,
	}, nil
}

type Application struct {
	p ApplicationParameters
}

func (a *Application) Run(ctx context.Context) error {
	grace := elephantine.NewGracefulShutdown(a.p.Logger, 10*time.Second)
	server := elephantine.NewAPIServer(a.p.Logger, a.p.Addr, a.p.ProfileAddr)

	opts, err := elephantine.NewDefaultServiceOptions(
		a.p.Logger, a.p.AuthInfoParser, a.p.Registerer,
		elephantine.ServiceAuthRequired,
	)
	if err != nil {
		return fmt.Errorf("set up service options: %w", err)
	}

	checkServer := user.NewMessagesServer(a.p.Service, opts.ServerOptions())

	server.RegisterAPI(checkServer, opts)

	grp := elephantine.NewErrGroup(ctx, a.p.Logger)

	grp.Go("server", func(ctx context.Context) error {
		return server.ListenAndServe(grace.CancelOnQuit(ctx))
	})

	return grp.Wait() //nolint:wrapcheck
}
