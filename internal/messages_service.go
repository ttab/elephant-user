package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	newsdoc_rpc "github.com/ttab/elephant-api/newsdoc"
	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephant-user/postgres"
	"github.com/ttab/elephantine/rpc"
)

// MessagesStore is the storage the Messages handlers need.
type MessagesStore interface {
	OnMessageUpdate(
		ctx context.Context, ch chan MessageEvent,
		recipient string, afterID int64,
	)
	OnInboxMessageUpdate(
		ctx context.Context, ch chan MessageEvent,
		recipient string, afterID int64,
	)
	GetLatestInboxMessageID(
		ctx context.Context, recipient string,
	) (int64, error)
	ListInboxMessagesBeforeID(
		ctx context.Context, recipient string,
		beforeID int64, size int64,
	) ([]InboxMessage, error)
	ListInboxMessagesAfterID(
		ctx context.Context, recipient string,
		afterID int64, size int64,
	) ([]InboxMessage, error)
	GetLatestMessageID(
		ctx context.Context, recipient string,
	) (int64, error)
	ListMessagesAfterID(
		ctx context.Context, recipient string,
		afterID int64, size int64,
	) ([]Message, error)
	InsertInboxMessage(
		ctx context.Context, message InboxMessage,
	) error
	InsertMessage(
		ctx context.Context, message Message,
	) error
	UpdateInboxMessage(
		ctx context.Context, recipient string,
		id int64, isRead bool,
	) error
	DeleteInboxMessage(
		ctx context.Context, recipient string, id int64,
	) error
}

type MessagesService struct {
	logger    *slog.Logger
	store     MessagesStore
	validator DocumentValidator
}

func NewMessagesService(
	logger *slog.Logger, store MessagesStore,
	validator DocumentValidator,
) *MessagesService {
	return &MessagesService{
		logger:    logger,
		store:     store,
		validator: validator,
	}
}

// Interface guard.
var _ user.Messages = &MessagesService{}

// PushMessage implements user.Messages.
func (s *MessagesService) PushMessage(
	ctx context.Context, req *user.PushMessageRequest,
) (*user.PushMessageResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	if req.Recipient == "" {
		return nil, rpc.RequiredArgument("recipient")
	}

	if req.Payload == nil {
		return nil, rpc.RequiredArgument("payload")
	}

	var docUUID *uuid.UUID

	if req.DocUuid != "" {
		parsed, err := uuid.Parse(req.DocUuid)
		if err != nil {
			return nil, rpc.InvalidArgument(
				"doc_uuid", err.Error())
		}

		docUUID = &parsed
	}

	payload, err := json.Marshal(req.Payload)
	if err != nil {
		return nil, rpc.Internalf("marshal message payload: %w", err)
	}

	err = s.store.InsertMessage(ctx, Message{
		Recipient: req.Recipient,
		Type:      req.Type,
		Created:   time.Now(),
		CreatedBy: auth.Claims.Subject,
		DocUUID:   docUUID,
		DocType:   req.DocType,
		Payload:   payload,
	})
	if err != nil {
		return nil, rpc.Internalf(
			"push message: %w", err)
	}

	return &user.PushMessageResponse{}, nil
}

// PushInboxMessage implements user.Messages.
func (s *MessagesService) PushInboxMessage(
	ctx context.Context, req *user.PushInboxMessageRequest,
) (*user.PushInboxMessageResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	if req.Recipient == "" {
		return nil, rpc.RequiredArgument("recipient")
	}

	if req.Payload == nil {
		return nil, rpc.RequiredArgument("payload")
	}

	newsdoc := newsdoc_rpc.DocumentFromRPC(req.Payload)

	validationResult, err := s.validator.ValidateDocument(
		ctx, postgres.SchemaUsageMessages, &newsdoc)
	if err != nil {
		return nil, rpc.Internalf("validate newsdoc payload: %w", err)
	}

	if len(validationResult) > 0 {
		return nil, validationError(validationResult)
	}

	payload, err := json.Marshal(req.Payload)
	if err != nil {
		return nil, rpc.Internalf("marshal message payload: %w", err)
	}

	now := time.Now()

	err = s.store.InsertInboxMessage(ctx, InboxMessage{
		Recipient: req.Recipient,
		Created:   now,
		CreatedBy: auth.Claims.Subject,
		Updated:   now,
		IsRead:    false,
		Payload:   payload,
	})
	if err != nil {
		return nil, rpc.Internalf(
			"push inbox message: %w", err)
	}

	return &user.PushInboxMessageResponse{}, nil
}

// PollMessages implements user.Messages.
func (s *MessagesService) PollMessages(
	ctx context.Context, req *user.PollMessagesRequest,
) (*user.PollMessagesResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	// Start listening for new messages.
	notifications := make(chan MessageEvent, 1)

	go s.store.OnMessageUpdate(
		ctx, notifications, auth.Claims.Subject, req.AfterId,
	)

	limit := int64(10)

	if req.AfterId == -1 {
		latestID, err := s.store.GetLatestMessageID(ctx, auth.Claims.Subject)
		if err != nil {
			return nil, rpc.Internalf(
				"get latest message id: %w", err)
		}

		req.AfterId = latestID
	}

	listMessages := func() ([]*user.Message, error) {
		msgs, err := s.store.ListMessagesAfterID(
			ctx, auth.Claims.Subject, req.AfterId, limit,
		)
		if err != nil {
			return nil, fmt.Errorf("after id %d: %w", req.AfterId, err)
		}

		var res []*user.Message

		for i := range msgs {
			docUUID := ""
			if msgs[i].DocUUID != nil {
				docUUID = msgs[i].DocUUID.String()
			}

			var payload map[string]string

			err = json.Unmarshal(msgs[i].Payload, &payload)
			if err != nil {
				return nil, fmt.Errorf("unmarshal payload: %w", err)
			}

			res = append(res, &user.Message{
				Recipient: msgs[i].Recipient,
				Id:        msgs[i].ID,
				Type:      msgs[i].Type,
				Created:   msgs[i].Created.Format(time.RFC3339),
				CreatedBy: msgs[i].CreatedBy,
				DocUuid:   docUUID,
				DocType:   msgs[i].DocType,
				Payload:   payload,
			})
		}

		return res, nil
	}

	// Check if there are already any messages available.
	msgs, err := listMessages()
	if err != nil {
		return nil, rpc.Internalf(
			"list messages: %w", err)
	}

	if len(msgs) > 0 {
		return &user.PollMessagesResponse{
			LastId:   msgs[len(msgs)-1].Id,
			Messages: msgs,
		}, nil
	}

	select {
	case <-notifications:
	case <-time.After(30 * time.Second):
	case <-ctx.Done():
		return nil, waitEndedError(ctx)
	}

	msgs, err = listMessages()
	if err != nil {
		return nil, rpc.Internalf(
			"list messages: %w", err)
	}

	lastID := req.AfterId
	if len(msgs) > 0 {
		lastID = msgs[len(msgs)-1].Id
	}

	return &user.PollMessagesResponse{
		LastId:   lastID,
		Messages: msgs,
	}, nil
}

// PollInboxMessages implements user.Messages.
func (s *MessagesService) PollInboxMessages(
	ctx context.Context, req *user.PollInboxMessagesRequest,
) (*user.PollInboxMessagesResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	// Start listening for new messages.
	notifications := make(chan MessageEvent, 1)

	go s.store.OnInboxMessageUpdate(
		ctx, notifications, auth.Claims.Subject, req.AfterId,
	)

	limit := int64(10)

	if req.AfterId == -1 {
		latestID, err := s.store.GetLatestInboxMessageID(ctx, auth.Claims.Subject)
		if err != nil {
			return nil, rpc.Internalf(
				"get latest message id: %w", err)
		}

		req.AfterId = latestID
	}

	listMessages := func() ([]*user.InboxMessage, error) {
		msgs, err := s.store.ListInboxMessagesAfterID(
			ctx, auth.Claims.Subject, req.AfterId, limit,
		)
		if err != nil {
			return nil, fmt.Errorf("after id %d: %w", req.AfterId, err)
		}

		var res []*user.InboxMessage

		for i := range msgs {
			updated := ""
			if !msgs[i].Updated.IsZero() {
				updated = msgs[i].Updated.Format(time.RFC3339)
			}

			payload, err := unmarshalInboxPayload(msgs[i].Payload)
			if err != nil {
				return nil, err
			}

			res = append(res, &user.InboxMessage{
				Recipient: msgs[i].Recipient,
				Id:        msgs[i].ID,
				Created:   msgs[i].Created.Format(time.RFC3339),
				CreatedBy: msgs[i].CreatedBy,
				Updated:   updated,
				IsRead:    msgs[i].IsRead,
				Payload:   payload,
			})
		}

		return res, nil
	}

	// Check if there are already any messages available.
	msgs, err := listMessages()
	if err != nil {
		return nil, rpc.Internalf(
			"list inbox messages: %w", err)
	}

	if len(msgs) > 0 {
		return &user.PollInboxMessagesResponse{
			LastId:   msgs[len(msgs)-1].Id,
			Messages: msgs,
		}, nil
	}

	select {
	case <-notifications:
	case <-time.After(30 * time.Second):
	case <-ctx.Done():
		return nil, waitEndedError(ctx)
	}

	msgs, err = listMessages()
	if err != nil {
		return nil, rpc.Internalf(
			"list inbox messages: %w", err)
	}

	lastID := req.AfterId
	if len(msgs) > 0 {
		lastID = msgs[len(msgs)-1].Id
	}

	return &user.PollInboxMessagesResponse{
		LastId:   lastID,
		Messages: msgs,
	}, nil
}

// ListInboxMessages implements user.Messages.
func (s *MessagesService) ListInboxMessages(
	ctx context.Context, req *user.ListInboxMessagesRequest,
) (*user.ListInboxMessagesResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	size := int64(10)
	if req.Size > 0 {
		size = req.Size
	}

	msgs, err := s.store.ListInboxMessagesBeforeID(
		ctx, auth.Claims.Subject, req.BeforeId, size,
	)
	if err != nil {
		return nil, rpc.Internalf(
			"list inbox messages: %w", err)
	}

	var res user.ListInboxMessagesResponse

	for i := range msgs {
		updated := ""
		if !msgs[i].Updated.IsZero() {
			updated = msgs[i].Updated.Format(time.RFC3339)
		}

		payload, err := unmarshalInboxPayload(msgs[i].Payload)
		if err != nil {
			return nil, rpc.Internalf(
				"list inbox messages: %w", err)
		}

		res.Messages = append(res.Messages, &user.InboxMessage{
			Recipient: msgs[i].Recipient,
			Id:        msgs[i].ID,
			Created:   msgs[i].Created.Format(time.RFC3339),
			CreatedBy: msgs[i].CreatedBy,
			Updated:   updated,
			IsRead:    msgs[i].IsRead,
			Payload:   payload,
		})
	}

	if len(msgs) > 0 {
		res.LatestId = msgs[0].ID
		res.EarliestId = msgs[len(msgs)-1].ID
	}

	return &res, nil
}

// UpdateInboxMessage implements user.Messages.
func (s *MessagesService) UpdateInboxMessage(
	ctx context.Context, req *user.UpdateInboxMessageRequest,
) (*user.UpdateInboxMessageResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	if req.Id < 1 {
		return nil, rpc.InvalidArgument("id",
			"cannot be less than 1")
	}

	err = s.store.UpdateInboxMessage(
		ctx, auth.Claims.Subject, req.Id, req.IsRead,
	)
	if err != nil {
		return nil, rpc.Internalf("update inbox message: %w", err)
	}

	return &user.UpdateInboxMessageResponse{}, nil
}

// DeleteInboxMessage implements user.Messages.
func (s *MessagesService) DeleteInboxMessage(
	ctx context.Context, req *user.DeleteInboxMessageRequest,
) (*user.DeleteInboxMessageResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	if req.Id < 1 {
		return nil, rpc.InvalidArgument("id",
			"cannot be less than 1")
	}

	err = s.store.DeleteInboxMessage(
		ctx, auth.Claims.Subject, req.Id,
	)
	if err != nil {
		return nil, rpc.Internalf("delete inbox message: %w", err)
	}

	return &user.DeleteInboxMessageResponse{}, nil
}

// unmarshalInboxPayload decodes a stored inbox message payload into the
// RPC document.
func unmarshalInboxPayload(raw json.RawMessage) (*newsdoc_rpc.Document, error) {
	var doc newsdoc_rpc.Document

	err := json.Unmarshal(raw, &doc)
	if err != nil {
		return nil, fmt.Errorf("unmarshal payload: %w", err)
	}

	return &doc, nil
}
