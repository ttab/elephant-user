package internal

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type MessageEvent struct {
	ID        int64
	Recipient string
}

// InboxMessage is an inbox message as the store holds it. Payload is the
// JSON encoded document, opaque to the store.
type InboxMessage struct {
	Recipient string
	ID        int64
	Created   time.Time
	CreatedBy string
	Updated   time.Time
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
