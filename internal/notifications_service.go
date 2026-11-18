package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	userv1 "github.com/ttab/elephant-api/elephant/user/v1"
	"github.com/ttab/elephant-api/elephant/user/v1/userv1connect"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/pg"
	"github.com/ttab/elephantine/rpc"
)

const (
	// notificationsDefaultLimit is how many rows ListNotifications
	// returns when the request does not say.
	notificationsDefaultLimit = 50
	// notificationsMaxLimit caps what ListNotifications returns in one
	// response. A request over the cap is served the cap.
	notificationsMaxLimit = 200
)

// notificationsNewest is the before time that lists from the newest row:
// later than any created time a replica's clock can stamp.
var notificationsNewest = time.Date(9999, time.December, 31, 0, 0, 0, 0, time.UTC)

// NotificationsStore is the storage the NotificationsService handlers need.
type NotificationsStore interface {
	InsertNotifications(
		ctx context.Context, event NotificationSourceEvent, keys []string,
	) ([]Notification, error)
	ListNotificationsBefore(
		ctx context.Context, keys []string,
		before time.Time, beforeKey string, beforeID int64, size int64,
	) ([]Notification, error)
	Subscribe(
		ctx context.Context, subject string, items []SubscriptionItem,
	) ([]Subscription, error)
	Unsubscribe(
		ctx context.Context, subject string, documentUUIDs []uuid.UUID,
	) error
	ListSubscriptions(
		ctx context.Context, subject string,
	) ([]Subscription, error)
	ListDocumentSubscribers(
		ctx context.Context, documentUUID uuid.UUID,
	) ([]string, error)
	ListNotificationsAfterID(
		ctx context.Context, key string, afterID int64,
		notBefore time.Time, size int64,
	) ([]Notification, error)
	GetNotificationCursors(
		ctx context.Context, subject string,
	) (map[string]int64, error)
	BumpNotificationCursor(
		ctx context.Context, subject string, key string, position int64,
	) error
}

// Interface guard.
var _ NotificationsStore = &PGStore{}

// NotificationsService implements the NotificationsService RPCs. Every RPC
// but Ingest needs the user scope; Ingest needs user_notify and nothing
// else will do. Stream is in notifications_stream.go.
type NotificationsService struct {
	logger        *slog.Logger
	store         NotificationsStore
	notifications *pg.FanOut[NotificationEvent]
	permissions   DocumentPermissions
	metrics       *Metrics

	streamKeepalive     time.Duration
	streamReplayDefault time.Duration
	streamReplayMax     time.Duration
}

// NewNotificationsService creates the service. notifications is the fan-out
// of the notification_update channel, which wakes the streams.
func NewNotificationsService(
	logger *slog.Logger, store NotificationsStore,
	notifications *pg.FanOut[NotificationEvent],
	permissions DocumentPermissions, metrics *Metrics,
	opts ...NotificationsServiceOption,
) *NotificationsService {
	s := NotificationsService{
		logger:        logger,
		store:         store,
		notifications: notifications,
		permissions:   permissions,
		metrics:       metrics,

		streamKeepalive:     DefaultStreamKeepalive,
		streamReplayDefault: DefaultStreamReplay,
		streamReplayMax:     DefaultStreamReplayMax,
	}

	for _, opt := range opts {
		opt(&s)
	}

	return &s
}

// Interface guard.
var _ userv1connect.NotificationsServiceHandler = &NotificationsService{}

// subscriptionTarget resolves the user a subscription RPC acts on: the
// caller when user is empty or the caller's own subject, else user, which
// must be a user uri: core://user/, core://application/ or
// keycloak://user/ followed by an id. The flag reports whether it is
// another user.
func subscriptionTarget(
	auth *elephantine.AuthInfo, user string,
) (string, bool, error) {
	if user == "" || user == auth.Claims.Subject {
		return auth.Claims.Subject, false, nil
	}

	if !isUserURI(user) {
		return "", false, rpc.InvalidArgumentf("user",
			"%q is not a user uri (%s..., %s... or %s...)", user,
			coreUserURIPrefix, coreApplicationURIPrefix,
			keycloakUserURIPrefix)
	}

	return user, true, nil
}

// requireWriteOnAll checks that the caller may write every document, as
// acting on another user's subscriptions needs. One refusal fails the call.
func (s *NotificationsService) requireWriteOnAll(
	ctx context.Context, documents []uuid.UUID,
) error {
	for _, id := range documents {
		err := requireDocumentAccess(ctx, s.permissions, id, accessWrite)
		if err != nil {
			return err
		}
	}

	return nil
}

// maxTTLSeconds is the largest ttl_seconds that fits in a time.Duration.
const maxTTLSeconds = math.MaxInt64 / int64(time.Second)

// subscriptionItemFromRPC parses an item and translates its lifetime: a
// positive ttl_seconds, an until in the future, permanent, or none for the
// store's default. Argument names are indexed, items.N.<field>.
func subscriptionItemFromRPC(
	i int, item *userv1.SubscriptionItem, now time.Time,
) (SubscriptionItem, error) {
	documentUUID, err := parseUUID(
		fmt.Sprintf("items.%d.document_uuid", i), item.GetDocumentUuid())
	if err != nil {
		return SubscriptionItem{}, err
	}

	res := SubscriptionItem{DocumentUUID: documentUUID}

	switch lifetime := item.GetLifetime().(type) {
	case nil:
	case *userv1.SubscriptionItem_TtlSeconds:
		argument := fmt.Sprintf("items.%d.ttl_seconds", i)

		if lifetime.TtlSeconds <= 0 {
			return res, rpc.InvalidArgument(argument, "must be positive")
		}

		if lifetime.TtlSeconds > maxTTLSeconds {
			return res, rpc.InvalidArgumentf(argument,
				"at most %d", maxTTLSeconds)
		}

		res.TTL = time.Duration(lifetime.TtlSeconds) * time.Second
	case *userv1.SubscriptionItem_Until:
		argument := fmt.Sprintf("items.%d.until", i)

		until, err := time.Parse(time.RFC3339, lifetime.Until)
		if err != nil {
			return res, rpc.InvalidArgument(argument, err.Error())
		}

		if !until.After(now) {
			return res, rpc.InvalidArgument(argument, "is not in the future")
		}

		res.Until = &until
	case *userv1.SubscriptionItem_Permanent:
		res.Permanent = lifetime.Permanent
	default:
		return res, rpc.InvalidArgumentf(fmt.Sprintf("items.%d", i),
			"unknown lifetime %T", lifetime)
	}

	return res, nil
}

// formatNotificationTime formats a feed timestamp as RFC3339 with its
// fractional seconds, the way the service promises.
func formatNotificationTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func subscriptionsToRPC(subs []Subscription) []*userv1.Subscription {
	res := make([]*userv1.Subscription, len(subs))

	for i, sub := range subs {
		var expires string

		if sub.Expires != nil {
			expires = formatNotificationTime(*sub.Expires)
		}

		res[i] = &userv1.Subscription{
			User:         sub.Subject,
			DocumentUuid: sub.DocumentUUID.String(),
			Expires:      expires,
		}
	}

	return res
}

// Subscribe implements [userv1connect.NotificationsServiceHandler].
func (s *NotificationsService) Subscribe(
	ctx context.Context, req *connect.Request[userv1.SubscribeRequest],
) (*connect.Response[userv1.SubscribeResponse], error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	subject, other, err := subscriptionTarget(auth, req.Msg.User)
	if err != nil {
		return nil, err
	}

	if len(req.Msg.Items) == 0 {
		return nil, rpc.RequiredArgument("items")
	}

	now := time.Now()

	items := make([]SubscriptionItem, len(req.Msg.Items))
	documents := make([]uuid.UUID, 0, len(req.Msg.Items))

	for i, item := range req.Msg.Items {
		parsed, err := subscriptionItemFromRPC(i, item, now)
		if err != nil {
			return nil, err
		}

		items[i] = parsed

		if !slices.Contains(documents, parsed.DocumentUUID) {
			documents = append(documents, parsed.DocumentUUID)
		}
	}

	if other {
		err := s.requireWriteOnAll(ctx, documents)
		if err != nil {
			return nil, err
		}
	}

	subs, err := s.store.Subscribe(ctx, subject, items)
	if errors.Is(err, ErrInvalidSubscriptionLifetime) {
		return nil, rpc.InvalidArgument("items", err.Error())
	}

	if err != nil {
		return nil, rpc.Internalf("subscribe: %w", err)
	}

	return connect.NewResponse(&userv1.SubscribeResponse{
		Subscriptions: subscriptionsToRPC(subs),
	}), nil
}

// Unsubscribe implements [userv1connect.NotificationsServiceHandler].
func (s *NotificationsService) Unsubscribe(
	ctx context.Context, req *connect.Request[userv1.UnsubscribeRequest],
) (*connect.Response[userv1.UnsubscribeResponse], error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	subject, other, err := subscriptionTarget(auth, req.Msg.User)
	if err != nil {
		return nil, err
	}

	if len(req.Msg.DocumentUuids) == 0 {
		return nil, rpc.RequiredArgument("document_uuids")
	}

	documents := make([]uuid.UUID, 0, len(req.Msg.DocumentUuids))

	for i, raw := range req.Msg.DocumentUuids {
		id, err := parseUUID(fmt.Sprintf("document_uuids.%d", i), raw)
		if err != nil {
			return nil, err
		}

		if !slices.Contains(documents, id) {
			documents = append(documents, id)
		}
	}

	if other {
		err := s.requireWriteOnAll(ctx, documents)
		if err != nil {
			return nil, err
		}
	}

	err = s.store.Unsubscribe(ctx, subject, documents)
	if err != nil {
		return nil, rpc.Internalf("unsubscribe: %w", err)
	}

	return connect.NewResponse(&userv1.UnsubscribeResponse{}), nil
}

// ListSubscriptions implements [userv1connect.NotificationsServiceHandler].
func (s *NotificationsService) ListSubscriptions(
	ctx context.Context, req *connect.Request[userv1.ListSubscriptionsRequest],
) (*connect.Response[userv1.ListSubscriptionsResponse], error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	subject, other, err := subscriptionTarget(auth, req.Msg.User)
	if err != nil {
		return nil, err
	}

	if other && !auth.Claims.HasScope(ScopeDocAdmin) {
		return nil, rpc.PermissionDeniedf(
			"listing another user's subscriptions needs the %s scope",
			ScopeDocAdmin)
	}

	subs, err := s.store.ListSubscriptions(ctx, subject)
	if err != nil {
		return nil, rpc.Internalf("list subscriptions: %w", err)
	}

	return connect.NewResponse(&userv1.ListSubscriptionsResponse{
		Subscriptions: subscriptionsToRPC(subs),
	}), nil
}

// ListNotifications implements [userv1connect.NotificationsServiceHandler].
// The caller's keys are its subject, its org and every unit in its token.
//
// Paging is on created alone, as the proto says: a page holds the rows
// created strictly before before. Rows that share the created time of the
// last row of a page, one event under several keys or two events in the
// same microsecond, are on neither side of the boundary when the page ends
// among them, so this list can skip such a row. The list is for the bell
// and the start screen; the stream carries the exact replay.
func (s *NotificationsService) ListNotifications(
	ctx context.Context, req *connect.Request[userv1.ListNotificationsRequest],
) (*connect.Response[userv1.ListNotificationsResponse], error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	if req.Msg.Limit < 0 {
		return nil, rpc.InvalidArgument("limit", "cannot be negative")
	}

	limit := int64(req.Msg.Limit)
	if limit == 0 {
		limit = notificationsDefaultLimit
	}

	limit = min(limit, notificationsMaxLimit)

	before := notificationsNewest

	if req.Msg.Before != "" {
		before, err = time.Parse(time.RFC3339, req.Msg.Before)
		if err != nil {
			return nil, rpc.InvalidArgument("before", err.Error())
		}
	}

	rows, err := s.store.ListNotificationsBefore(
		ctx, getAllOwners(auth), before, "", 0, limit)
	if err != nil {
		return nil, rpc.Internalf("list notifications: %w", err)
	}

	res := userv1.ListNotificationsResponse{
		Notifications: make([]*userv1.Notification, len(rows)),
	}

	for i, n := range rows {
		res.Notifications[i] = notificationToRPC(n)
	}

	return connect.NewResponse(&res), nil
}

func notificationToRPC(n Notification) *userv1.Notification {
	refs := make([]*userv1.Reference, len(n.Refs))

	for i, r := range n.Refs {
		refs[i] = &userv1.Reference{
			Rel:  r.Rel,
			Uuid: r.UUID.String(),
			Type: r.Type,
		}
	}

	return &userv1.Notification{
		Key:          n.Key,
		Id:           n.ID,
		Kind:         n.Kind,
		DocumentUuid: n.DocumentUUID.String(),
		DocumentType: n.DocumentType,
		Refs:         refs,
		Actor:        n.Actor,
		EventUuid:    n.EventUUID.String(),
		Created:      formatNotificationTime(n.Created),
		Meta:         n.Meta,
	}
}

// Ingest implements [userv1connect.NotificationsServiceHandler]. Without a
// recipient the event is written under every live subscriber of the
// document, and a document without subscribers writes nothing; with one it
// is written under that key only, which must be a user uri in one of its
// three forms (see isUserURI), a core://unit/ uri or a core://org/ uri. A
// repeated event_uuid writes nothing new under a key that has it.
func (s *NotificationsService) Ingest(
	ctx context.Context, req *connect.Request[userv1.IngestRequest],
) (*connect.Response[userv1.IngestResponse], error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUserNotify)
	if err != nil {
		return nil, err
	}

	msg := req.Msg

	documentUUID, err := parseUUID("document_uuid", msg.DocumentUuid)
	if err != nil {
		return nil, err
	}

	if msg.Kind == "" {
		return nil, rpc.RequiredArgument("kind")
	}

	err = checkReverseDomainNotation(msg.Kind)
	if err != nil {
		return nil, rpc.InvalidArgument("kind", err.Error())
	}

	eventUUID, err := parseUUID("event_uuid", msg.EventUuid)
	if err != nil {
		return nil, err
	}

	if msg.Recipient != "" && !isFeedKey(msg.Recipient) {
		return nil, rpc.InvalidArgumentf("recipient",
			"%q is not a user, unit or org uri", msg.Recipient)
	}

	refs := make([]NotificationReference, len(msg.Refs))

	for i, r := range msg.Refs {
		if r.GetRel() == "" {
			return nil, rpc.RequiredArgument(fmt.Sprintf("refs.%d.rel", i))
		}

		refUUID, err := parseUUID(fmt.Sprintf("refs.%d.uuid", i), r.GetUuid())
		if err != nil {
			return nil, err
		}

		refs[i] = NotificationReference{
			Rel:  r.GetRel(),
			UUID: refUUID,
			Type: r.GetType(),
		}
	}

	actor := msg.Actor
	if actor == "" {
		actor = auth.Claims.Subject
	}

	keys := []string{msg.Recipient}

	if msg.Recipient == "" {
		keys, err = s.store.ListDocumentSubscribers(ctx, documentUUID)
		if err != nil {
			return nil, rpc.Internalf("list document subscribers: %w", err)
		}
	}

	written, err := s.store.InsertNotifications(ctx, NotificationSourceEvent{
		Family:       NotificationFamilyIngest,
		Kind:         msg.Kind,
		DocumentUUID: documentUUID,
		DocumentType: msg.DocumentType,
		Refs:         refs,
		Actor:        actor,
		EventUUID:    eventUUID,
		Meta:         msg.Meta,
	}, keys)
	if err != nil {
		return nil, rpc.Internalf("insert notifications: %w", err)
	}

	s.logger.DebugContext(ctx, "ingested notification event",
		"event_uuid", eventUUID,
		"document_uuid", documentUUID,
		"kind", msg.Kind,
		"keys", len(keys),
		"written", len(written))

	return connect.NewResponse(&userv1.IngestResponse{}), nil
}
