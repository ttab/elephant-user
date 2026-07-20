package internal

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/ttab/elephant-user/postgres"
)

var (
	ErrGenerationNotFound = errors.New("config generation not found")
	ErrSchemaNotFound     = errors.New("schema not found")
	ErrSchemaMismatch     = errors.New(
		"schema already exists with a different spec or usage")
	ErrSchemaSpecMissing = errors.New("schema spec missing")
)

// ConfigSchema is a named and versioned revisor constraint set together
// with the usage it's accepted for. It's used both for stored schemas
// and for the schemas of a config generation. Spec may be nil where
// specs aren't loaded or supplied.
type ConfigSchema struct {
	Name    string
	Version string
	Spec    json.RawMessage
	Usage   postgres.SchemaUsage
}

// ConfigGeneration is a set of schemas that are active together.
type ConfigGeneration struct {
	ID          int64
	Description string
	Active      bool
	Created     time.Time
	Activated   *time.Time
	Schemas     []ConfigSchema
}

// Deprecation is the enforcement status for a deprecation label.
type Deprecation struct {
	Label    string
	Enforced bool
}

// SchemaEvent is the notification payload for active config generation
// changes.
type SchemaEvent struct {
	Type string `json:"type"`
}

// DeprecationEvent is the notification payload for deprecation changes.
type DeprecationEvent struct {
	Label string `json:"label"`
}
