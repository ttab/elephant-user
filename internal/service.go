package internal

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/ttab/elephant-api/newsdoc"
	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephantine"
	"github.com/twitchtv/twirp"
)

const ScopeUser = "user"

type MessageEvent struct {
	ID        int64
	Recipient string
}

type InboxMessage struct {
	Recipient string
	ID        int64
	Created   time.Time
	CreatedBy string
	Updated   time.Time
	Payload   *newsdoc.Document
}

type Message struct {
	Recipient string
	ID        int64
	Type      string
	Created   time.Time
	CreatedBy string
	DocUUID   *uuid.UUID
	DocType   string
	Payload   map[string]string
}

type MessageType string

const (
	MessageTypeSystem MessageType = "system"
	MessageTypeInbox  MessageType = "inbox"
)

type Store interface {
	OnMessageUpdate(
		ctx context.Context, ch chan MessageEvent,
		recipient string, afterID int64,
	)
	OnInboxMessageUpdate(
		ctx context.Context, ch chan MessageEvent,
		recipient string, afterID int64,
	)
	ListInboxMessages(
		ctx context.Context, recipient string,
		beforeID int64, size int64,
	) ([]InboxMessage, error)
	InsertInboxMessage(
		ctx context.Context, message InboxMessage,
	) error
	InsertMessage(
		ctx context.Context, message Message,
	) error
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
	ctx context.Context, req *user.ListInboxMessagesRequest,
) (*user.ListInboxMessagesResponse, error) {
	auth, err := elephantine.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	size := int64(10)
	if req.Size > 0 {
		size = req.Size
	}

	msgs, err := s.store.ListInboxMessages(
		ctx, auth.Claims.Subject, req.BeforeId, size,
	)
	if err != nil {
		return nil, twirp.InternalErrorf(
			"list inbox messages: %w", err)
	}

	var res user.ListInboxMessagesResponse

	for i := range msgs {
		updated := ""
		if !msgs[i].Updated.IsZero() {
			updated = msgs[i].Updated.Format(time.RFC3339)
		}

		res.Messages = append(res.Messages, &user.InboxMessage{
			Recipient: msgs[i].Recipient,
			Id:        msgs[i].ID,
			Created:   msgs[i].Created.Format(time.RFC3339),
			CreatedBy: msgs[i].CreatedBy,
			Updated:   updated,
			Payload:   msgs[i].Payload,
		})
	}

	return &res, nil
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
	ctx context.Context, req *user.PushInboxMessageRequest,
) (*user.PushInboxMessageResponse, error) {
	auth, err := elephantine.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	if req.Recipient == "" {
		return nil, twirp.RequiredArgumentError("recipient")
	}

	if req.Payload == nil {
		return nil, twirp.RequiredArgumentError("payload")
	}

	// TODO: Validate payload.
	now := time.Now()

	err = s.store.InsertInboxMessage(ctx, InboxMessage{
		Recipient: req.Recipient,
		Created:   now,
		CreatedBy: auth.Claims.Subject,
		Updated:   now,
		Payload:   req.Payload,
	})
	if err != nil {
		return nil, twirp.InternalErrorf(
			"failed to push inbox message: %w", err)
	}

	return &user.PushInboxMessageResponse{}, nil
}

// PushMessage implements user.Messages.
func (s *Service) PushMessage(
	ctx context.Context, req *user.PushMessageRequest,
) (*user.PushMessageResponse, error) {
	auth, err := elephantine.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	if req.Recipient == "" {
		return nil, twirp.RequiredArgumentError("recipient")
	}

	if req.Payload == nil {
		return nil, twirp.RequiredArgumentError("payload")
	}

	var docUUID *uuid.UUID

	if req.DocUuid != "" {
		parsed, err := uuid.Parse(req.DocUuid)
		if err != nil {
			return nil, twirp.InvalidArgumentError("doc_uuid", err.Error())
		}

		docUUID = &parsed
	}

	err = s.store.InsertMessage(ctx, Message{
		Recipient: req.Recipient,
		Type:      req.Type,
		Created:   time.Now(),
		CreatedBy: auth.Claims.Subject,
		DocUUID:   docUUID,
		DocType:   req.DocType,
		Payload:   req.Payload,
	})
	if err != nil {
		return nil, twirp.InternalErrorf(
			"failed to push inbox message: %w", err)
	}

	return &user.PushMessageResponse{}, nil
}

// UpdateInboxMessage implements user.Messages.
func (s *Service) UpdateInboxMessage(
	context.Context, *user.UpdateInboxMessageRequest,
) (*user.UpdateInboxMessageResponse, error) {
	panic("unimplemented")
}
