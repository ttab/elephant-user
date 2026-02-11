package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	newsdoc_rpc "github.com/ttab/elephant-api/newsdoc"
	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephant-user/postgres"
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

type EventLogEvent struct {
	ID    int64
	Owner string
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

type Document struct {
	Owner         string
	Application   string
	Type          string
	Key           string
	Version       int64
	SchemaVersion string
	Title         string
	Created       time.Time
	Updated       time.Time
	UpdatedBy     string
	Payload       []byte
}

type DocumentUpdate struct {
	Owner         string
	Application   string
	Type          string
	Key           string
	SchemaVersion string
	Title         string
	UpdatedBy     string
	Payload       []byte
}

type EventLogEntry struct {
	ID           int64
	Owner        string
	Type         postgres.EventType
	ResourceKind postgres.ResourceKind
	Application  string
	DocumentType string
	Key          string
	Version      int64
	UpdatedBy    string
	Created      time.Time
	Payload      []byte
}

func mapEventType(t postgres.EventType) user.EventLogEntryType {
	switch t {
	case postgres.EventTypeUpdate:
		return user.EventLogEntryType_EVENT_LOG_ENTRY_UPDATE
	case postgres.EventTypeDelete:
		return user.EventLogEntryType_EVENT_LOG_ENTRY_DELETE
	default:
		return user.EventLogEntryType_EVENT_LOG_ENTRY_UNSPECIFIED
	}
}

func mapResourceKind(k postgres.ResourceKind) user.ResourceKind {
	switch k {
	case postgres.ResourceKindDocument:
		return user.ResourceKind_RESOURCE_KIND_DOCUMENT
	case postgres.ResourceKindProperty:
		return user.ResourceKind_RESOURCE_KIND_PROPERTY
	default:
		return user.ResourceKind_RESOURCE_KIND_UNSPECIFIED
	}
}

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
	GetDocument(
		ctx context.Context, owner string, application string,
		docType string, key string,
	) (*Document, error)
	ListDocuments(
		ctx context.Context, owner string, application string,
		docType string, includePayload bool,
	) ([]*Document, error)
	UpdateDocument(
		ctx context.Context, update DocumentUpdate,
	) error
	DeleteDocument(
		ctx context.Context, owner string, application string,
		docType string, key string,
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
	GetLatestEventLogID(
		ctx context.Context, owner string,
	) (int64, error)
	GetEventLogEntriesAfterID(
		ctx context.Context, owner string,
		afterID int64, limit int64,
	) ([]EventLogEntry, error)
	OnEventLogUpdate(
		ctx context.Context, ch chan EventLogEvent,
		owner string, afterID int64,
	)
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
	ctx context.Context, req *user.GetDocumentRequest,
) (*user.GetDocumentResponse, error) {
	auth, err := elephantine.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	targetOwner := auth.Claims.Subject
	if req.Owner != "" {
		// TODO: Add logic to check if `sub` is allowed to read req.Owner's docs.
		targetOwner = req.Owner
	}

	doc, err := s.store.GetDocument(ctx, targetOwner, req.Application, req.Type, req.Key)
	if errors.Is(err, ErrDocNotFound) {
		return nil, twirp.NotFoundError("no such document")
	} else if err != nil {
		return nil, twirp.InternalErrorf("get document: %w", err)
	}

	var newsdoc newsdoc_rpc.Document

	err = json.Unmarshal(doc.Payload, &newsdoc)
	if err != nil {
		return nil, fmt.Errorf("unmarshal payload: %w", err)
	}

	return &user.GetDocumentResponse{
		Document: &user.Document{
			Owner:       doc.Owner,
			Application: doc.Application,
			Type:        doc.Type,
			Key:         doc.Key,
			Version:     doc.Version,
			// TODO: Admins will be able to edit all documents.
			ReadOnly:      doc.Owner != auth.Claims.Subject,
			SchemaVersion: doc.SchemaVersion,
			Title:         doc.Title,
			Created:       doc.Created.Format(time.RFC3339),
			Updated:       doc.Updated.Format(time.RFC3339),
			UpdatedBy:     doc.UpdatedBy,
			Payload:       &newsdoc,
		},
	}, nil
}

// ListDocuments implements [user.Settings].
func (s *Service) ListDocuments(
	ctx context.Context, req *user.ListDocumentsRequest,
) (*user.ListDocumentsResponse, error) {
	auth, err := elephantine.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	// TODO: Logic to list shared documents (where owner is org and user's units) would go here.

	docs, err := s.store.ListDocuments(ctx,
		auth.Claims.Subject, req.Application,
		req.Type, req.IncludePayload,
	)
	if err != nil {
		return nil, twirp.InternalErrorf("list documents: %w", err)
	}

	res := make([]*user.Document, len(docs))

	for i, d := range docs {
		var newsdoc newsdoc_rpc.Document

		if req.IncludePayload && len(d.Payload) > 0 {
			err = json.Unmarshal(docs[i].Payload, &newsdoc)
			if err != nil {
				return nil, fmt.Errorf("unmarshal payload: %w", err)
			}
		}

		res[i] = &user.Document{
			Owner:         d.Owner,
			Application:   d.Application,
			Type:          d.Type,
			Key:           d.Key,
			Version:       d.Version,
			SchemaVersion: d.SchemaVersion,
			// TODO: Admins will be able to edit all documents.
			ReadOnly:  d.Owner != auth.Claims.Subject,
			Title:     d.Title,
			Created:   d.Created.Format(time.RFC3339),
			Updated:   d.Updated.Format(time.RFC3339),
			UpdatedBy: d.UpdatedBy,
			Payload:   &newsdoc,
		}
	}

	return &user.ListDocumentsResponse{
		Documents: res,
	}, nil
}

// UpdateDocument implements [user.Settings].
func (s *Service) UpdateDocument(
	ctx context.Context, req *user.UpdateDocumentRequest,
) (*user.UpdateDocumentResponse, error) {
	auth, err := elephantine.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	targetOwner := auth.Claims.Subject
	if req.Owner != "" {
		// TODO: Add admin check here if targetOwner != `sub`.
		targetOwner = req.Owner
	}

	if req.Payload == nil {
		return nil, twirp.RequiredArgumentError("payload")
	}

	newsdoc := newsdoc_rpc.DocumentFromRPC(req.Payload)

	// Add nil uuid.UUID to satisfy the validator.
	newsdoc.UUID = uuid.UUID{}.String()

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

	payload, err := json.Marshal(req.Payload)
	if err != nil {
		return nil, fmt.Errorf("marshal document payload: %w", err)
	}

	err = s.store.UpdateDocument(ctx, DocumentUpdate{
		Owner:         targetOwner,
		Application:   req.Application,
		Type:          req.Type,
		Key:           req.Key,
		SchemaVersion: req.SchemaVersion,
		Title:         newsdoc.Title,
		UpdatedBy:     auth.Claims.Subject,
		Payload:       payload,
	})
	if err != nil {
		return nil, twirp.InternalErrorf("update document: %w", err)
	}

	return &user.UpdateDocumentResponse{}, nil
}

// DeleteDocument implements [user.Settings].
func (s *Service) DeleteDocument(
	ctx context.Context, req *user.DeleteDocumentRequest,
) (*user.DeleteDocumentResponse, error) {
	auth, err := elephantine.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	targetOwner := auth.Claims.Subject
	if req.Owner != "" {
		// TODO: Add admin check here if targetOwner != `sub`.
		targetOwner = req.Owner
	}

	err = s.store.DeleteDocument(ctx, targetOwner, req.Application, req.Type, req.Key)
	if err != nil {
		return nil, twirp.InternalErrorf("delete document: %w", err)
	}

	return &user.DeleteDocumentResponse{}, nil
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
	ctx context.Context, req *user.PollEventLogRequest,
) (*user.PollEventLogResponse, error) {
	auth, err := elephantine.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	// Start listening for setting updates.
	notifications := make(chan EventLogEvent, 1)

	go s.store.OnEventLogUpdate(
		ctx, notifications, auth.Claims.Subject, req.AfterId,
	)

	limit := int64(10)

	if req.AfterId == -1 {
		latestID, err := s.store.GetLatestEventLogID(ctx, auth.Claims.Subject)
		if err != nil {
			return nil, twirp.InternalErrorf(
				"get latest message id: %w", err)
		}

		req.AfterId = latestID
	}

	// Helper function to fetch and map events.
	listLogEntries := func() ([]*user.EventLogEntry, int64, error) {
		events, err := s.store.GetEventLogEntriesAfterID(ctx, auth.Claims.Subject, req.AfterId, limit)
		if err != nil {
			return nil, 0, err //nolint:wrapcheck
		}

		maxID := req.AfterId
		entries := make([]*user.EventLogEntry, len(events))

		for i, e := range events {
			entries[i] = &user.EventLogEntry{
				Id:           e.ID,
				Created:      e.Created.Format(time.RFC3339),
				Owner:        e.Owner,
				Application:  e.Application,
				DocumentType: e.DocumentType,
				Key:          e.Key,
				Version:      e.Version,
				UpdatedBy:    e.UpdatedBy,
				Type:         mapEventType(e.Type),
				Kind:         mapResourceKind(e.ResourceKind),
			}

			if e.ID > maxID {
				maxID = e.ID
			}
		}

		return entries, maxID, nil
	}

	// If the client is behind, we don't want to wait.
	events, lastID, err := listLogEntries()
	if err != nil {
		return nil, twirp.InternalErrorf("list log entries: %w", err)
	}

	if len(events) > 0 {
		return &user.PollEventLogResponse{
			LastId:  lastID,
			Entries: events,
		}, nil
	}

	select {
	case <-notifications:
	case <-time.After(30 * time.Second):
	case <-ctx.Done():
		return nil, fmt.Errorf("client disconnected: %w", ctx.Err())
	}

	events, lastID, err = listLogEntries()
	if err != nil {
		return nil, twirp.InternalErrorf("list log entries: %w", err)
	}

	return &user.PollEventLogResponse{
		LastId:  lastID,
		Entries: events,
	}, nil
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
