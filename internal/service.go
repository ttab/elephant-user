package internal

import (
	"context"
	"log/slog"

	"github.com/ttab/elephant-api/user"
)

type MessageEvent struct {
	ID        int64
	Recipient string
}

type Store interface {
	OnMessageUpdate(
		ctx context.Context, ch chan MessageEvent,
		recipient string, afterID int64,
	)
	OnInboxMessageUpdate(
		ctx context.Context, ch chan MessageEvent,
		recipient string, afterID int64,
	)
}

type Service struct {
	logger *slog.Logger
	store  Store
}

func NewService(
	logger *slog.Logger, store Store,
) *Service {
	return &Service{
		logger: logger,
		store:  store,
	}
}

// Interface guard.
var _ user.Messages = &Service{}

// DeleteInboxMessage implements user.Messages.
func (s *Service) DeleteInboxMessage(
	context.Context, *user.DeleteInboxMessageRequest,
) (*user.DeleteInboxMessageResponse, error) {
	panic("unimplemented")
}

// ListInboxMessages implements user.Messages.
func (s *Service) ListInboxMessages(
	context.Context, *user.ListInboxMessagesRequest,
) (*user.ListInboxMessagesResponse, error) {
	panic("unimplemented")
}

// PollInboxMessages implements user.Messages.
func (s *Service) PollInboxMessages(
	context.Context, *user.PollInboxMessagesRequest,
) (*user.PollInboxMessagesResponse, error) {
	panic("unimplemented")
}

// PollMessages implements user.Messages.
func (s *Service) PollMessages(
	context.Context, *user.PollMessagesRequest,
) (*user.PollMessagesResponse, error) {
	panic("unimplemented")
}

// PushInboxMessage implements user.Messages.
func (s *Service) PushInboxMessage(
	context.Context, *user.PushInboxMessageRequest,
) (*user.PushInboxMessageResponse, error) {
	panic("unimplemented")
}

// PushMessage implements user.Messages.
func (s *Service) PushMessage(
	context.Context, *user.PushMessageRequest,
) (*user.PushMessageResponse, error) {
	panic("unimplemented")
}

// UpdateInboxMessage implements user.Messages.
func (s *Service) UpdateInboxMessage(
	context.Context, *user.UpdateInboxMessageRequest,
) (*user.UpdateInboxMessageResponse, error) {
	panic("unimplemented")
}
