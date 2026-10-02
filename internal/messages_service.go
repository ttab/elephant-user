package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	newsdoc_rpc "github.com/ttab/elephant-api/newsdoc"
	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephant-user/postgres"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/rpc"
)

// MessagesStore is the storage the Messages handlers need.
type MessagesStore interface {
	OnMessageUpdate(
		ctx context.Context, ch chan MessageEvent,
		recipient string, afterID int64,
	)
	GetLatestMessageID(
		ctx context.Context, recipient string,
	) (int64, error)
	ListMessagesAfterID(
		ctx context.Context, recipient string,
		afterID int64, size int64,
	) ([]Message, error)
	InsertMessage(
		ctx context.Context, message Message,
	) error

	OnInboxMessageUpdate(
		ctx context.Context, ch chan MessageEvent,
		reader InboxReader, afterID int64,
	)
	GetLatestInboxMessageID(
		ctx context.Context, reader InboxReader,
	) (int64, error)
	ListInboxMessagesBeforeID(
		ctx context.Context, reader InboxReader,
		beforeID int64, size int64,
	) ([]InboxMessage, error)
	ListInboxMessagesAfterID(
		ctx context.Context, reader InboxReader,
		afterID int64, size int64,
	) ([]InboxMessage, error)
	InsertInboxMessage(
		ctx context.Context, message InboxMessage,
	) (int64, error)
	SetInboxMessageRead(
		ctx context.Context, reader InboxReader, id int64,
		isRead bool, updated time.Time,
	) error
	HideInboxMessage(
		ctx context.Context, reader InboxReader, id int64,
		updated time.Time,
	) error
}

type MessagesService struct {
	store     MessagesStore
	validator DocumentValidator
}

func NewMessagesService(
	store MessagesStore, validator DocumentValidator,
) *MessagesService {
	return &MessagesService{
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

// inboxReader is the caller as an inbox reader: everything addressed to
// their sub, org or units is theirs to read, under their own read state.
func inboxReader(auth *elephantine.AuthInfo) InboxReader {
	return InboxReader{
		Owners:  getAllOwners(auth),
		Subject: auth.Claims.Subject,
	}
}

// isGroupRecipient reports whether an inbox recipient is a unit or an org
// rather than a user. Anything that is not a unit or an org follows the
// user model, service subjects included.
func isGroupRecipient(recipient string) bool {
	return strings.HasPrefix(recipient, "core://unit/") ||
		strings.HasPrefix(recipient, "core://org/")
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

	// A unit or org recipient follows the shared-write rule for settings
	// documents: the doc_admin scope and membership of the target.
	if isGroupRecipient(req.Recipient) {
		if !auth.Claims.HasScope(ScopeDocAdmin) {
			return nil, rpc.PermissionDeniedf(
				"only admins can push messages to a unit or org")
		}

		if !isAllowedOwner(auth, req.Recipient) {
			return nil, rpc.PermissionDeniedf(
				"not allowed to push messages to %q", req.Recipient)
		}
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

	// The validator has already required a well-formed uuid; this parse
	// is what the store needs and a guard against that ever changing.
	docUUID, err := uuid.Parse(req.Payload.Uuid)
	if err != nil {
		return nil, rpc.InvalidArgument("payload.uuid", err.Error())
	}

	payload, err := json.Marshal(req.Payload)
	if err != nil {
		return nil, rpc.Internalf("marshal message payload: %w", err)
	}

	id, err := s.store.InsertInboxMessage(ctx, InboxMessage{
		UUID:      docUUID,
		Recipient: req.Recipient,
		Created:   time.Now(),
		CreatedBy: auth.Claims.Subject,
		Payload:   payload,
	})
	if errors.Is(err, ErrInboxMessageConflict) {
		return nil, rpc.AlreadyExists(fmt.Sprintf(
			"a different message with uuid %s already exists for %q",
			req.Payload.Uuid, req.Recipient))
	}

	if err != nil {
		return nil, rpc.Internalf("push inbox message: %w", err)
	}

	return &user.PushInboxMessageResponse{Id: id}, nil
}

// PollInboxMessages implements user.Messages.
func (s *MessagesService) PollInboxMessages(
	ctx context.Context, req *user.PollInboxMessagesRequest,
) (*user.PollInboxMessagesResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	reader := inboxReader(auth)
	limit := clampSize(req.Size)

	// Start listening for new messages.
	notifications := make(chan MessageEvent, 1)

	go s.store.OnInboxMessageUpdate(
		ctx, notifications, reader, req.AfterId,
	)

	if req.AfterId == -1 {
		latestID, err := s.store.GetLatestInboxMessageID(ctx, reader)
		if err != nil {
			return nil, rpc.Internalf(
				"get latest inbox message id: %w", err)
		}

		req.AfterId = latestID
	}

	listMessages := func() ([]*user.InboxMessage, error) {
		msgs, err := s.store.ListInboxMessagesAfterID(
			ctx, reader, req.AfterId, limit,
		)
		if err != nil {
			return nil, fmt.Errorf("after id %d: %w", req.AfterId, err)
		}

		var res []*user.InboxMessage

		for i := range msgs {
			m, err := inboxMessageToRPC(msgs[i])
			if err != nil {
				return nil, err
			}

			res = append(res, m)
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

	msgs, err := s.store.ListInboxMessagesBeforeID(
		ctx, inboxReader(auth), req.BeforeId, clampSize(req.Size),
	)
	if err != nil {
		return nil, rpc.Internalf(
			"list inbox messages: %w", err)
	}

	var res user.ListInboxMessagesResponse

	for i := range msgs {
		m, err := inboxMessageToRPC(msgs[i])
		if err != nil {
			return nil, rpc.Internalf(
				"list inbox messages: %w", err)
		}

		res.Messages = append(res.Messages, m)
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

	err = s.store.SetInboxMessageRead(
		ctx, inboxReader(auth), req.Id, req.IsRead, time.Now(),
	)
	if errors.Is(err, ErrInboxMessageNotFound) {
		return nil, rpc.NotFound("no such inbox message")
	}

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

	err = s.store.HideInboxMessage(
		ctx, inboxReader(auth), req.Id, time.Now(),
	)
	if errors.Is(err, ErrInboxMessageNotFound) {
		return nil, rpc.NotFound("no such inbox message")
	}

	if err != nil {
		return nil, rpc.Internalf("delete inbox message: %w", err)
	}

	return &user.DeleteInboxMessageResponse{}, nil
}

// inboxMessageToRPC maps a stored inbox message to its RPC shape. Messages
// are not changed after creation, so updated equals created; the reader's
// state is not reflected in it.
func inboxMessageToRPC(msg InboxMessage) (*user.InboxMessage, error) {
	payload, err := unmarshalDocument(msg.Payload)
	if err != nil {
		return nil, err
	}

	created := msg.Created.Format(time.RFC3339)

	return &user.InboxMessage{
		Recipient: msg.Recipient,
		Id:        msg.ID,
		Created:   created,
		CreatedBy: msg.CreatedBy,
		Updated:   created,
		IsRead:    msg.IsRead,
		Payload:   payload,
	}, nil
}
