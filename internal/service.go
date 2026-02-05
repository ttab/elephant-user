package internal

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	newsdoc_rpc "github.com/ttab/elephant-api/newsdoc"
	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephantine"
	"github.com/ttab/newsdoc"
	"github.com/ttab/revisor"
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
	IsRead    bool
	Payload   *newsdoc_rpc.Document
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

type Property struct {
	Owner       string
	Application string
	Key         string
	Value       string
	Created     time.Time
	Updated     time.Time
}

type PropertyUpdate struct {
	Application string
	Key         string
	Value       string
}

type PropertyDelete struct {
	Application string
	Key         string
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
	GetProperties(
		ctx context.Context, owner string,
		application string, keys []string,
	) ([]Property, error)
	SetProperties(
		ctx context.Context, owner string,
		updates []PropertyUpdate,
	) error
	DeleteProperties(
		ctx context.Context, owner string,
		deletes []PropertyDelete,
	) error
}

type DocumentValidator interface {
	ValidateDocument(
		ctx context.Context, document *newsdoc.Document,
	) ([]revisor.ValidationResult, error)
}

type Service struct {
	logger    *slog.Logger
	store     Store
	validator DocumentValidator
}

func NewService(
	logger *slog.Logger, store Store,
	validator DocumentValidator,
) *Service {
	return &Service{
		logger:    logger,
		store:     store,
		validator: validator,
	}
}

// Interface guard.
var _ user.Messages = &Service{}

// GetDocument implements [user.Settings].
func (s *Service) GetDocument(
	context.Context, *user.GetDocumentRequest,
) (*user.GetDocumentResponse, error) {
	panic("unimplemented")
}

// ListDocuments implements [user.Settings].
func (s *Service) ListDocuments(
	context.Context, *user.ListDocumentsRequest,
) (*user.ListDocumentsResponse, error) {
	panic("unimplemented")
}

// UpdateDocument implements [user.Settings].
func (s *Service) UpdateDocument(
	context.Context, *user.UpdateDocumentRequest,
) (*user.UpdateDocumentResponse, error) {
	panic("unimplemented")
}

// DeleteDocument implements [user.Settings].
func (s *Service) DeleteDocument(
	context.Context, *user.DeleteDocumentRequest,
) (*user.DeleteDocumentResponse, error) {
	panic("unimplemented")
}

// GetProperties implements [user.Settings].
func (s *Service) GetProperties(
	ctx context.Context, req *user.GetPropertiesRequest,
) (*user.GetPropertiesResponse, error) {
	auth, err := elephantine.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	props, err := s.store.GetProperties(ctx, auth.Claims.Subject, req.Application, req.Keys)
	if err != nil {
		return nil, twirp.InternalErrorf("get properties: %w", err)
	}

	var res user.GetPropertiesResponse

	for i := range props {
		res.Properties = append(res.Properties, &user.Property{
			Owner:       props[i].Owner,
			Application: props[i].Application,
			Key:         props[i].Key,
			Value:       props[i].Value,
			Created:     props[i].Created.Format(time.RFC3339),
			Updated:     props[i].Updated.Format(time.RFC3339),
		})
	}

	return &res, nil
}

// SetProperties implements [user.Settings].
func (s *Service) SetProperties(
	ctx context.Context, req *user.SetPropertiesRequest,
) (*user.SetPropertiesResponse, error) {
	auth, err := elephantine.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	updates := make([]PropertyUpdate, len(req.Properties))
	for i, p := range req.Properties {
		updates[i] = PropertyUpdate{
			Application: p.Application,
			Key:         p.Key,
			Value:       p.Value,
		}
	}

	err = s.store.SetProperties(ctx, auth.Claims.Subject, updates)
	if err != nil {
		return nil, twirp.InternalErrorf("set properties: %w", err)
	}

	return &user.SetPropertiesResponse{}, nil
}

// DeleteProperties implements [user.Settings].
func (s *Service) DeleteProperties(
	ctx context.Context, req *user.DeletePropertiesRequest,
) (*user.DeletePropertiesResponse, error) {
	auth, err := elephantine.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	deletes := make([]PropertyDelete, len(req.Properties))
	for i, p := range req.Properties {
		deletes[i] = PropertyDelete{
			Application: p.Application,
			Key:         p.Key,
		}
	}

	err = s.store.DeleteProperties(ctx, auth.Claims.Subject, deletes)
	if err != nil {
		return nil, twirp.InternalErrorf("delete properties: %w", err)
	}

	return &user.DeletePropertiesResponse{}, nil
}

// PollEventLog implements [user.Settings].
func (s *Service) PollEventLog(
	context.Context, *user.PollEventLogRequest,
) (*user.PollEventLogResponse, error) {
	panic("unimplemented")
}

// DeleteInboxMessage implements user.Messages.
func (s *Service) DeleteInboxMessage(
	ctx context.Context, req *user.DeleteInboxMessageRequest,
) (*user.DeleteInboxMessageResponse, error) {
	auth, err := elephantine.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	if req.Id < 1 {
		return nil, twirp.InvalidArgumentError("id",
			"cannot be less than 1")
	}

	err = s.store.DeleteInboxMessage(
		ctx, auth.Claims.Subject, req.Id,
	)
	if err != nil {
		return nil, twirp.InternalErrorf("delete inbox message: %w", err)
	}

	return &user.DeleteInboxMessageResponse{}, nil
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

	msgs, err := s.store.ListInboxMessagesBeforeID(
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
			IsRead:    msgs[i].IsRead,
			Payload:   msgs[i].Payload,
		})
	}

	if len(msgs) > 0 {
		res.LatestId = msgs[0].ID
		res.EarliestId = msgs[len(msgs)-1].ID
	}

	return &res, nil
}

// PollInboxMessages implements user.Messages.
func (s *Service) PollInboxMessages(
	ctx context.Context, req *user.PollInboxMessagesRequest,
) (*user.PollInboxMessagesResponse, error) {
	auth, err := elephantine.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err //nolint:wrapcheck
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
			return nil, twirp.InternalErrorf(
				"get latest message id: %w", err)
		}

		req.AfterId = latestID
	}

	listMessages := func() ([]*user.InboxMessage, error) {
		msgs, err := s.store.ListInboxMessagesAfterID(
			ctx, auth.Claims.Subject, req.AfterId, limit,
		)
		if err != nil {
			return nil, err //nolint:wrapcheck
		}

		var res []*user.InboxMessage

		for i := range msgs {
			updated := ""
			if !msgs[i].Updated.IsZero() {
				updated = msgs[i].Updated.Format(time.RFC3339)
			}

			res = append(res, &user.InboxMessage{
				Recipient: msgs[i].Recipient,
				Id:        msgs[i].ID,
				Created:   msgs[i].Created.Format(time.RFC3339),
				CreatedBy: msgs[i].CreatedBy,
				Updated:   updated,
				IsRead:    msgs[i].IsRead,
				Payload:   msgs[i].Payload,
			})
		}

		return res, nil
	}

	// Check if there are already any messages available.
	msgs, err := listMessages()
	if err != nil {
		return nil, twirp.InternalErrorf(
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
	}

	msgs, err = listMessages()
	if err != nil {
		return nil, twirp.InternalErrorf(
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

// PollMessages implements user.Messages.
func (s *Service) PollMessages(
	ctx context.Context, req *user.PollMessagesRequest,
) (*user.PollMessagesResponse, error) {
	auth, err := elephantine.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err //nolint:wrapcheck
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
			return nil, twirp.InternalErrorf(
				"get latest message id: %w", err)
		}

		req.AfterId = latestID
	}

	listMessages := func() ([]*user.Message, error) {
		msgs, err := s.store.ListMessagesAfterID(
			ctx, auth.Claims.Subject, req.AfterId, limit,
		)
		if err != nil {
			return nil, err //nolint:wrapcheck
		}

		var res []*user.Message

		for i := range msgs {
			docUUID := ""
			if msgs[i].DocUUID != nil {
				docUUID = msgs[i].DocUUID.String()
			}

			res = append(res, &user.Message{
				Recipient: msgs[i].Recipient,
				Id:        msgs[i].ID,
				Type:      msgs[i].Type,
				Created:   msgs[i].Created.Format(time.RFC3339),
				CreatedBy: msgs[i].CreatedBy,
				DocUuid:   docUUID,
				DocType:   msgs[i].DocType,
				Payload:   msgs[i].Payload,
			})
		}

		return res, nil
	}

	// Check if there are already any messages available.
	msgs, err := listMessages()
	if err != nil {
		return nil, twirp.InternalErrorf(
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
	}

	msgs, err = listMessages()
	if err != nil {
		return nil, twirp.InternalErrorf(
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

	newsdoc := newsdoc_rpc.DocumentFromRPC(req.Payload)

	validationResult, err := s.validator.ValidateDocument(ctx, &newsdoc)
	if err != nil {
		return nil, fmt.Errorf("validate newsdoc payload: %w", err)
	}

	if len(validationResult) > 0 {
		err := twirp.InvalidArgument.Errorf(
			"the document had %d validation errors, the first one is: %v",
			len(validationResult), validationResult[0].String())

		err = err.WithMeta("err_count",
			strconv.Itoa(len(validationResult)))

		for i := range validationResult {
			err = err.WithMeta(strconv.Itoa(i),
				validationResult[i].String())
		}

		return nil, err
	}

	now := time.Now()

	err = s.store.InsertInboxMessage(ctx, InboxMessage{
		Recipient: req.Recipient,
		Created:   now,
		CreatedBy: auth.Claims.Subject,
		Updated:   now,
		IsRead:    false,
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
			return nil, twirp.InvalidArgumentError(
				"doc_uuid", err.Error())
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
	ctx context.Context, req *user.UpdateInboxMessageRequest,
) (*user.UpdateInboxMessageResponse, error) {
	auth, err := elephantine.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	if req.Id < 1 {
		return nil, twirp.InvalidArgumentError("id",
			"cannot be less than 1")
	}

	err = s.store.UpdateInboxMessage(
		ctx, auth.Claims.Subject, req.Id, req.IsRead,
	)
	if err != nil {
		return nil, twirp.InternalErrorf("update inbox message: %w", err)
	}

	return &user.UpdateInboxMessageResponse{}, nil
}
