package internal

import (
	"errors"
	"time"

	"github.com/ttab/elephant-user/postgres"
)

var ErrDocNotFound = errors.New("document not found")

type EventLogEvent struct {
	ID    int64
	Owner string
}

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
