package internal

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrInboxMessageConflict is returned when a message with the same payload
// uuid is already stored for the recipient but with a different payload.
var ErrInboxMessageConflict = errors.New(
	"a different message with that uuid already exists for the recipient")

// ErrInboxMessageNotFound is returned when no inbox message with the given
// id is addressed to any of the reader's owners.
var ErrInboxMessageNotFound = errors.New("inbox message not found")

type MessageEvent struct {
	ID        int64
	Recipient string
}

// InboxStateEvent announces a change to one reader's state for an inbox
// message. Nothing in this service listens to it yet; it exists for the
// notification stream that will.
type InboxStateEvent struct {
	ID        int64  `json:"id"`
	Recipient string `json:"recipient"`
	Subject   string `json:"subject"`
	CreatedBy string `json:"created_by"`
}

// InboxReader identifies who is reading the inbox: the owners the caller's
// claims match (sub, org, units), which decide what is addressed to them,
// and the subject their read state is kept under.
type InboxReader struct {
	Owners  []string
	Subject string
}

// InboxMessage is an inbox message as the store holds it. UUID is the
// payload document's uuid and identifies the message per recipient. Payload
// is the JSON encoded document, opaque to the store. IsRead is the reading
// subject's state when the message was read from the store, false on
// insert.
type InboxMessage struct {
	ID        int64
	UUID      uuid.UUID
	Recipient string
	Created   time.Time
	CreatedBy string
	IsRead    bool
	Payload   json.RawMessage
}

// Message is a system message as the store holds it. Payload is the JSON
// encoded key/value payload, opaque to the store.
type Message struct {
	Recipient string
	ID        int64
	Type      string
	Created   time.Time
	CreatedBy string
	DocUUID   *uuid.UUID
	DocType   string
	Payload   json.RawMessage
}

type MessageType string

const (
	MessageTypeSystem MessageType = "system"
	MessageTypeInbox  MessageType = "inbox"
)
