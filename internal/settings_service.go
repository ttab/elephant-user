package internal

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	newsdoc_rpc "github.com/ttab/elephant-api/newsdoc"
	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephant-user/postgres"
	"github.com/ttab/elephantine/rpc"
)

// SettingsStore is the storage the Settings handlers need.
type SettingsStore interface {
	GetDocument(
		ctx context.Context, owner string, application string,
		docType string, key string,
	) (*Document, error)
	ListDocuments(
		ctx context.Context, owners []string, application string,
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
		ctx context.Context, owners []string,
	) (int64, error)
	GetEventLogEntriesAfterID(
		ctx context.Context, owners []string,
		afterID int64, limit int64,
	) ([]EventLogEntry, error)
	OnEventLogUpdate(
		ctx context.Context, ch chan EventLogEvent,
		owners []string, afterID int64,
	)
}

type SettingsService struct {
	store     SettingsStore
	validator DocumentValidator
}

func NewSettingsService(
	store SettingsStore, validator DocumentValidator,
) *SettingsService {
	return &SettingsService{
		store:     store,
		validator: validator,
	}
}

// Interface guard.
var _ user.Settings = &SettingsService{}

// GetDocument implements [user.Settings].
func (s *SettingsService) GetDocument(
	ctx context.Context, req *user.GetDocumentRequest,
) (*user.GetDocumentResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	targetOwner := auth.Claims.Subject
	if req.Owner != "" {
		if !isAllowedOwner(auth, req.Owner) {
			return nil, rpc.PermissionDeniedf(
				"not allowed to read documents for %q", req.Owner)
		}

		targetOwner = req.Owner
	}

	doc, err := s.store.GetDocument(ctx, targetOwner, req.Application, req.Type, req.Key)
	if errors.Is(err, ErrDocNotFound) {
		return nil, rpc.NotFound("no such document")
	} else if err != nil {
		return nil, rpc.Internalf("get document: %w", err)
	}

	newsdoc, err := unmarshalDocument(doc.Payload)
	if err != nil {
		return nil, rpc.Internalf("%w", err)
	}

	return &user.GetDocumentResponse{
		Document: &user.Document{
			Owner:         doc.Owner,
			Application:   doc.Application,
			Type:          doc.Type,
			Key:           doc.Key,
			Version:       doc.Version,
			ReadOnly:      doc.Owner != auth.Claims.Subject && !auth.Claims.HasScope(ScopeDocAdmin),
			SchemaVersion: doc.SchemaVersion,
			Title:         doc.Title,
			Created:       doc.Created.Format(time.RFC3339),
			Updated:       doc.Updated.Format(time.RFC3339),
			UpdatedBy:     doc.UpdatedBy,
			Payload:       newsdoc,
		},
	}, nil
}

// ListDocuments implements [user.Settings].
func (s *SettingsService) ListDocuments(
	ctx context.Context, req *user.ListDocumentsRequest,
) (*user.ListDocumentsResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	owners := getAllOwners(auth)

	docs, err := s.store.ListDocuments(ctx,
		owners, req.Application,
		req.Type, req.IncludePayload,
	)
	if err != nil {
		return nil, rpc.Internalf("list documents: %w", err)
	}

	res := make([]*user.Document, len(docs))

	for i, d := range docs {
		newsdoc := &newsdoc_rpc.Document{}

		if req.IncludePayload && len(d.Payload) > 0 {
			newsdoc, err = unmarshalDocument(docs[i].Payload)
			if err != nil {
				return nil, rpc.Internalf("%w", err)
			}
		}

		res[i] = &user.Document{
			Owner:         d.Owner,
			Application:   d.Application,
			Type:          d.Type,
			Key:           d.Key,
			Version:       d.Version,
			SchemaVersion: d.SchemaVersion,
			ReadOnly:      d.Owner != auth.Claims.Subject && !auth.Claims.HasScope(ScopeDocAdmin),
			Title:         d.Title,
			Created:       d.Created.Format(time.RFC3339),
			Updated:       d.Updated.Format(time.RFC3339),
			UpdatedBy:     d.UpdatedBy,
			Payload:       newsdoc,
		}
	}

	return &user.ListDocumentsResponse{
		Documents: res,
	}, nil
}

// UpdateDocument implements [user.Settings].
func (s *SettingsService) UpdateDocument(
	ctx context.Context, req *user.UpdateDocumentRequest,
) (*user.UpdateDocumentResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	targetOwner := auth.Claims.Subject
	if req.Owner != "" {
		if req.Owner != auth.Claims.Subject {
			if !auth.Claims.HasScope(ScopeDocAdmin) {
				return nil, rpc.PermissionDeniedf(
					"only admins can update documents for other owners")
			}

			if !isAllowedOwner(auth, req.Owner) {
				return nil, rpc.PermissionDeniedf(
					"not allowed to update documents for %q", req.Owner)
			}
		}

		targetOwner = req.Owner
	}

	if req.Payload == nil {
		return nil, rpc.RequiredArgument("payload")
	}

	newsdoc := newsdoc_rpc.DocumentFromRPC(req.Payload)

	// Add nil uuid.UUID to satisfy the validator.
	newsdoc.UUID = uuid.UUID{}.String()

	validationResult, err := s.validator.ValidateDocument(
		ctx, postgres.SchemaUsageSettings, &newsdoc)
	if err != nil {
		return nil, rpc.Internalf("validate newsdoc payload: %w", err)
	}

	if len(validationResult) > 0 {
		return nil, validationError(validationResult)
	}

	payload, err := json.Marshal(req.Payload)
	if err != nil {
		return nil, rpc.Internalf("marshal document payload: %w", err)
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
		return nil, rpc.Internalf("update document: %w", err)
	}

	return &user.UpdateDocumentResponse{}, nil
}

// DeleteDocument implements [user.Settings].
func (s *SettingsService) DeleteDocument(
	ctx context.Context, req *user.DeleteDocumentRequest,
) (*user.DeleteDocumentResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	targetOwner := auth.Claims.Subject
	if req.Owner != "" {
		if req.Owner != auth.Claims.Subject {
			if !auth.Claims.HasScope(ScopeDocAdmin) {
				return nil, rpc.PermissionDeniedf(
					"only admins can delete documents for other owners")
			}

			if !isAllowedOwner(auth, req.Owner) {
				return nil, rpc.PermissionDeniedf(
					"not allowed to delete documents for %q", req.Owner)
			}
		}

		targetOwner = req.Owner
	}

	err = s.store.DeleteDocument(ctx, targetOwner, req.Application, req.Type, req.Key)
	if err != nil {
		return nil, rpc.Internalf("delete document: %w", err)
	}

	return &user.DeleteDocumentResponse{}, nil
}

// GetProperties implements [user.Settings].
func (s *SettingsService) GetProperties(
	ctx context.Context, req *user.GetPropertiesRequest,
) (*user.GetPropertiesResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	props, err := s.store.GetProperties(ctx, auth.Claims.Subject, req.Application, req.Keys)
	if err != nil {
		return nil, rpc.Internalf("get properties: %w", err)
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
func (s *SettingsService) SetProperties(
	ctx context.Context, req *user.SetPropertiesRequest,
) (*user.SetPropertiesResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
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
		return nil, rpc.Internalf("set properties: %w", err)
	}

	return &user.SetPropertiesResponse{}, nil
}

// DeleteProperties implements [user.Settings].
func (s *SettingsService) DeleteProperties(
	ctx context.Context, req *user.DeletePropertiesRequest,
) (*user.DeletePropertiesResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
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
		return nil, rpc.Internalf("delete properties: %w", err)
	}

	return &user.DeletePropertiesResponse{}, nil
}

// PollEventLog implements [user.Settings].
func (s *SettingsService) PollEventLog(
	ctx context.Context, req *user.PollEventLogRequest,
) (*user.PollEventLogResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	owners := getAllOwners(auth)

	// Start listening for setting updates.
	notifications := make(chan EventLogEvent, 1)

	go s.store.OnEventLogUpdate(
		ctx, notifications, owners, req.AfterId,
	)

	limit := int64(10)

	if req.AfterId == -1 {
		latestID, err := s.store.GetLatestEventLogID(ctx, owners)
		if err != nil {
			return nil, rpc.Internalf(
				"get latest message id: %w", err)
		}

		req.AfterId = latestID
	}

	// Helper function to fetch and map events.
	listLogEntries := func() ([]*user.EventLogEntry, int64, error) {
		events, err := s.store.GetEventLogEntriesAfterID(ctx, owners, req.AfterId, limit)
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
		return nil, rpc.Internalf("list log entries: %w", err)
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
		return nil, waitEndedError(ctx)
	}

	events, lastID, err = listLogEntries()
	if err != nil {
		return nil, rpc.Internalf("list log entries: %w", err)
	}

	return &user.PollEventLogResponse{
		LastId:  lastID,
		Entries: events,
	}, nil
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
