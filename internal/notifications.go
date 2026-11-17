package internal

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// DefaultSubscriptionTTL is how long a subscription lives when the
// subscriber gives no lifetime of its own.
const DefaultSubscriptionTTL = 24 * time.Hour

// DefaultNotificationRetention is how long the cleaner keeps feed rows: six
// months, like the inbox.
const DefaultNotificationRetention = 180 * 24 * time.Hour

// ErrInvalidSubscriptionLifetime is returned for a subscription item that
// gives more than one of a TTL, an until time and the permanent flag, or a
// TTL that is not positive.
var ErrInvalidSubscriptionLifetime = errors.New(
	"a subscription takes at most one of a positive ttl, an until time and permanent")

// NotificationFamily is the part of the system an event comes from. It
// labels the feed metrics and is never stored.
type NotificationFamily string

const (
	NotificationFamilyIngest   NotificationFamily = "ingest"
	NotificationFamilyComments NotificationFamily = "comments"
	NotificationFamilySettings NotificationFamily = "settings"
	NotificationFamilyInbox    NotificationFamily = "inbox"
	NotificationFamilyMessages NotificationFamily = "messages"
)

// Notification is one feed row: an event under one key, a user, unit or
// org uri. Key and ID identify the row, and ids are commit-ordered within
// the key. EventUUID identifies the event, which can have rows under
// several keys. Everything but Meta is a reference, never a body.
type Notification struct {
	Key          string
	ID           int64
	Kind         string
	DocumentUUID uuid.UUID
	DocumentType string
	Refs         []NotificationReference
	Actor        string
	EventUUID    uuid.UUID
	Created      time.Time
	Meta         map[string]string
}

// NotificationReference is something else an event refers to, such as a
// thread, a comment or an assignment.
type NotificationReference struct {
	Rel  string    `json:"rel"`
	UUID uuid.UUID `json:"uuid"`
	Type string    `json:"type,omitempty"`
}

// NotificationSourceEvent is the event a producer hands to the fan-out,
// which writes it as one row per key. Family says which producer it is.
// Meta is producer-supplied facts for display.
type NotificationSourceEvent struct {
	Family       NotificationFamily
	Kind         string
	DocumentUUID uuid.UUID
	DocumentType string
	Refs         []NotificationReference
	Actor        string
	EventUUID    uuid.UUID
	Meta         map[string]string
}

// NotificationEvent announces a feed row on the notification_update
// channel. It is a wakeup: readers re-read the row by key and id.
type NotificationEvent struct {
	Key string `json:"key"`
	ID  int64  `json:"id"`
}

// SubscriptionItem asks for a subscription to a document. At most one of
// TTL, Until and Permanent is given; none means the store's default TTL.
type SubscriptionItem struct {
	DocumentUUID uuid.UUID
	TTL          time.Duration
	Until        *time.Time
	Permanent    bool
}

// Subscription is a subject's interest in a document: document events
// become rows under the subject's key while it lives. Expires is nil for a
// permanent subscription.
type Subscription struct {
	Subject      string
	DocumentUUID uuid.UUID
	Expires      *time.Time
}
