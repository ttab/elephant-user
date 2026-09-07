package internal

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ttab/elephant-user/postgres"
	"github.com/ttab/elephantine"
	"github.com/ttab/newsdoc"
	"github.com/ttab/revisor"
)

const (
	LogKeyDeprecationLabel = "deprecation_label"
	LogKeyEntityRef        = "entity_ref"
)

//go:embed se.ecms.user.messages.json
var schemaMessages []byte

//go:embed se.ecms.user.settings.json
var schemaSettings []byte

// EmbeddedConfigSchemas returns the embedded constraint sets as
// register-ready config schemas. Used to seed environments and tests.
func EmbeddedConfigSchemas() []ConfigSchema {
	return []ConfigSchema{
		{
			Name:    "se.ecms.user.settings",
			Version: "v1.0.0",
			Spec:    schemaSettings,
			Usage:   postgres.SchemaUsageSettings,
		},
		{
			Name:    "se.ecms.user.messages",
			Version: "v1.0.0",
			Spec:    schemaMessages,
			Usage:   postgres.SchemaUsageMessages,
		},
	}
}

// ValidatorStore is the storage interface the validator loads schemas
// and deprecations from.
type ValidatorStore interface {
	GetActiveSchemas(ctx context.Context) ([]ConfigSchema, error)
	GetActiveConfigGenerationID(ctx context.Context) (int64, error)
	OnSchemaUpdate(ctx context.Context, ch chan SchemaEvent)
	GetEnforcedDeprecations(ctx context.Context) (map[string]bool, error)
	OnDeprecationUpdate(ctx context.Context, ch chan DeprecationEvent)
}

// Validator validates documents against the active config generation,
// with one revisor validator per schema usage. Validators are rebuilt
// when the active generation or deprecations change.
type Validator struct {
	m                    sync.RWMutex
	validators           map[postgres.SchemaUsage]*revisor.Validator
	activeGenerationID   int64
	enforcedDeprecations map[string]bool

	logger  *slog.Logger
	metrics *Metrics

	cancel      func()
	stopChannel chan struct{}
	refreshChan chan chan struct{}
}

// NewValidator creates a validator that loads its schemas from the
// store and reloads them when notified of changes (with a periodic
// fallback).
func NewValidator(
	ctx context.Context, logger *slog.Logger, store ValidatorStore,
	metrics *Metrics,
) (*Validator, error) {
	ctx, cancel := context.WithCancel(ctx)

	v := Validator{
		logger:      logger,
		metrics:     metrics,
		cancel:      cancel,
		stopChannel: make(chan struct{}),
		refreshChan: make(chan chan struct{}),
	}

	err := v.loadSchemas(ctx, store)
	if err != nil {
		cancel()

		return nil, fmt.Errorf("load schemas: %w", err)
	}

	err = v.loadDeprecations(ctx, store)
	if err != nil {
		cancel()

		return nil, fmt.Errorf("load deprecations: %w", err)
	}

	go v.reloadLoop(ctx, store)

	return &v, nil
}

// RefreshSchemas synchronously triggers a reload of schemas and
// deprecations.
func (v *Validator) RefreshSchemas(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	ch := make(chan struct{})

	select {
	case v.refreshChan <- ch:
	case <-ctx.Done():
		return fmt.Errorf("request refresh: %w", ctx.Err())
	}

	select {
	case <-ch:
	case <-ctx.Done():
		return fmt.Errorf("wait for refresh: %w", ctx.Err())
	}

	return nil
}

// Stop the validator reload loop.
func (v *Validator) Stop() {
	v.cancel()
	<-v.stopChannel
}

// ActiveGenerationID returns the ID of the config generation the
// current validators were built from.
func (v *Validator) ActiveGenerationID() int64 {
	v.m.RLock()
	defer v.m.RUnlock()

	return v.activeGenerationID
}

// ValidateDocument validates a document against the active schemas for
// the given usage.
func (v *Validator) ValidateDocument(
	ctx context.Context, usage postgres.SchemaUsage, doc *newsdoc.Document,
) ([]revisor.ValidationResult, error) {
	v.m.RLock()
	val := v.validators[usage]
	v.m.RUnlock()

	if val == nil {
		return nil, fmt.Errorf("no active schema for usage %q", usage)
	}

	res, err := val.ValidateDocument(ctx, doc,
		revisor.WithDeprecationHandler(v.deprecationHandler))
	if err != nil {
		return nil, fmt.Errorf("validate document: %w", err)
	}

	return res, nil
}

func (v *Validator) deprecationHandler(
	ctx context.Context, doc *newsdoc.Document,
	deprecation revisor.Deprecation,
	deprecationContext revisor.DeprecationContext,
) (revisor.DeprecationDecision, error) {
	v.m.RLock()
	enforced := v.enforcedDeprecations[deprecation.Label]
	v.m.RUnlock()

	if !enforced {
		var entityRef string

		if deprecationContext.Entity != nil {
			entityRef = deprecationContext.Entity.String()
		}

		v.logger.WarnContext(ctx, "use of deprecated value",
			elephantine.LogKeyDocumentUUID, doc.UUID,
			LogKeyDeprecationLabel, deprecation.Label,
			LogKeyEntityRef, entityRef)

		v.metrics.Deprecations.WithLabelValues(deprecation.Label).Inc()
		v.metrics.DocsWithDeprecations.WithLabelValues(doc.Type).Inc()
	}

	return revisor.DeprecationDecision{
		Enforce: enforced,
	}, nil
}

func (v *Validator) reloadLoop(ctx context.Context, store ValidatorStore) {
	defer close(v.stopChannel)

	recheckInterval := 5 * time.Minute

	schemaSub := make(chan SchemaEvent, 1)
	deprecationSub := make(chan DeprecationEvent, 1)

	store.OnSchemaUpdate(ctx, schemaSub)
	store.OnDeprecationUpdate(ctx, deprecationSub)

	for {
		var refreshChan chan struct{}

		select {
		case <-ctx.Done():
			return
		case <-time.After(recheckInterval):
		case <-schemaSub:
		case <-deprecationSub:
		case refreshChan = <-v.refreshChan:
		}

		err := v.loadSchemas(ctx, store)
		if err != nil {
			v.logger.ErrorContext(ctx, "refresh schemas",
				elephantine.LogKeyError, err,
				elephantine.LogKeyCountMetric,
				"elephant_user_schema_refresh_failure_count")
		}

		err = v.loadDeprecations(ctx, store)
		if err != nil {
			v.logger.ErrorContext(ctx, "refresh deprecations",
				elephantine.LogKeyError, err,
				elephantine.LogKeyCountMetric,
				"elephant_user_deprecation_refresh_failure_count")
		}

		if refreshChan != nil {
			close(refreshChan)
		}
	}
}

// loadSchemas builds one revisor validator per usage from the active
// config generation and swaps them in. On failure the previous
// validators are kept.
func (v *Validator) loadSchemas(ctx context.Context, store ValidatorStore) error {
	schemas, err := store.GetActiveSchemas(ctx)
	if err != nil {
		return fmt.Errorf("get active generation schemas: %w", err)
	}

	generationID, err := store.GetActiveConfigGenerationID(ctx)
	if err != nil {
		return fmt.Errorf("get active generation id: %w", err)
	}

	grouped := make(map[postgres.SchemaUsage][]revisor.ConstraintSet)

	for _, schema := range schemas {
		var cs revisor.ConstraintSet

		err := json.Unmarshal(schema.Spec, &cs)
		if err != nil {
			return fmt.Errorf("decode schema %s@%s: %w",
				schema.Name, schema.Version, err)
		}

		grouped[schema.Usage] = append(grouped[schema.Usage], cs)
	}

	validators := make(
		map[postgres.SchemaUsage]*revisor.Validator, len(grouped))

	for usage, sets := range grouped {
		val, err := revisor.NewValidator(sets...)
		if err != nil {
			return fmt.Errorf(
				"create validator for usage %q: %w", usage, err)
		}

		validators[usage] = val
	}

	v.m.Lock()
	v.validators = validators
	v.activeGenerationID = generationID
	v.m.Unlock()

	return nil
}

func (v *Validator) loadDeprecations(
	ctx context.Context, store ValidatorStore,
) error {
	deprecations, err := store.GetEnforcedDeprecations(ctx)
	if err != nil {
		return fmt.Errorf("get enforced deprecations: %w", err)
	}

	v.m.Lock()
	v.enforcedDeprecations = deprecations
	v.m.Unlock()

	return nil
}
