package internal

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephantine"
)

type Parameters struct {
	Logger         *slog.Logger
	APIServer      *elephantine.APIServer
	AuthInfoParser elephantine.AuthInfoParser
	Registerer     prometheus.Registerer
	Service        *Service
}

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

	p.APIServer.RegisterAPI(messagesServer, opts)

	grp := elephantine.NewErrGroup(ctx, p.Logger)

	grp.Go("server", func(ctx context.Context) error {
		return p.APIServer.ListenAndServe(grace.CancelOnQuit(ctx))
	})

	return grp.Wait() //nolint:wrapcheck
}
