package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephant-user/postgres"
	"github.com/ttab/elephantine"
	"github.com/ttab/revisor"
	"github.com/twitchtv/twirp"
)

const (
	ScopeSchemaAdmin = "schema_admin"
	ScopeSchemaRead  = "schema_read"
)

// ConfigurationStore is the storage interface used by the
// configuration service.
type ConfigurationStore interface {
	RegisterConfigGeneration(
		ctx context.Context, description string,
		schemas []ConfigSchema, activate bool,
	) (*ConfigGeneration, error)
	ActivateConfigGeneration(
		ctx context.Context, id int64,
	) (*ConfigGeneration, error)
	GetActiveConfigGeneration(ctx context.Context) (*ConfigGeneration, error)
	GetActiveConfigGenerationID(ctx context.Context) (int64, error)
	ListConfigGenerations(
		ctx context.Context, before int64, pageSize int64,
	) ([]*ConfigGeneration, error)
	GetSchema(
		ctx context.Context, name string, version string,
	) (*ConfigSchema, error)
	GetDeprecations(ctx context.Context) ([]Deprecation, error)
	UpdateDeprecation(ctx context.Context, deprecation Deprecation) error
	OnSchemaUpdate(ctx context.Context, ch chan SchemaEvent)
}

// ConfigurationService implements the user.Configuration Twirp service.
type ConfigurationService struct {
	logger *slog.Logger
	store  ConfigurationStore
}

// Interface guard.
var _ user.Configuration = &ConfigurationService{}

func NewConfigurationService(
	logger *slog.Logger, store ConfigurationStore,
) *ConfigurationService {
	return &ConfigurationService{
		logger: logger,
		store:  store,
	}
}

// RegisterConfigGeneration implements user.Configuration.
func (s *ConfigurationService) RegisterConfigGeneration(
	ctx context.Context, req *user.RegisterConfigGenerationRequest,
) (*user.RegisterConfigGenerationResponse, error) {
	_, err := elephantine.RequireAnyScope(ctx, ScopeSchemaAdmin)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	if len(req.Schemas) == 0 {
		return nil, twirp.RequiredArgumentError("schemas")
	}

	schemas := make([]ConfigSchema, len(req.Schemas))
	seen := make(map[string]bool, len(req.Schemas))

	// Constraint sets grouped by usage for the validator dry-run.
	grouped := make(map[postgres.SchemaUsage][]revisor.ConstraintSet)

	for i, schema := range req.Schemas {
		if schema.Name == "" {
			return nil, twirp.RequiredArgumentError(
				fmt.Sprintf("schemas.%d.name", i))
		}

		if schema.Version == "" {
			return nil, twirp.RequiredArgumentError(
				fmt.Sprintf("schemas.%d.version", i))
		}

		if seen[schema.Name] {
			return nil, twirp.InvalidArgument.Errorf(
				"schemas.%d.name: %q listed twice",
				i, schema.Name)
		}

		seen[schema.Name] = true

		usage, err := schemaUsageFromRPC(schema.Usage)
		if err != nil {
			return nil, twirp.InvalidArgument.Errorf(
				"schema %s@%s: %v",
				schema.Name, schema.Version, err)
		}

		cs, err := s.resolveConstraintSet(ctx, schema)
		if err != nil {
			return nil, err
		}

		grouped[usage] = append(grouped[usage], cs)

		schemas[i] = ConfigSchema{
			Name:    schema.Name,
			Version: schema.Version,
			Spec:    []byte(schema.Spec),
			Usage:   usage,
		}
	}

	// Dry-run the validators before persisting anything: activating
	// a generation that can't build a validator would make every
	// subsequent schema reload fail.
	for usage, sets := range grouped {
		_, err := revisor.NewValidator(sets...)
		if err != nil {
			return nil, twirp.InvalidArgument.Errorf(
				"the schemas for usage %q cannot form a valid constraint set: %v",
				usage, err)
		}
	}

	gen, err := s.store.RegisterConfigGeneration(
		ctx, req.Description, schemas, req.Activate)
	if errors.Is(err, ErrSchemaMismatch) || errors.Is(err, ErrSchemaSpecMissing) {
		return nil, twirp.InvalidArgument.Error(err.Error())
	} else if err != nil {
		return nil, twirp.InternalErrorf("register generation: %v", err)
	}

	return &user.RegisterConfigGenerationResponse{
		Generation: configGenerationToRPC(gen),
	}, nil
}

// resolveConstraintSet decodes the supplied schema spec, or loads the
// stored spec when none is supplied.
func (s *ConfigurationService) resolveConstraintSet(
	ctx context.Context, schema *user.ConfigGenerationSchema,
) (revisor.ConstraintSet, error) {
	var cs revisor.ConstraintSet

	if schema.Spec != "" {
		dec := json.NewDecoder(bytes.NewReader([]byte(schema.Spec)))

		dec.DisallowUnknownFields()

		err := dec.Decode(&cs)
		if err != nil {
			return cs, twirp.InvalidArgument.Errorf(
				"invalid spec for schema %s@%s: %v",
				schema.Name, schema.Version, err)
		}

		return cs, nil
	}

	stored, err := s.store.GetSchema(ctx, schema.Name, schema.Version)
	if errors.Is(err, ErrSchemaNotFound) {
		return cs, twirp.InvalidArgument.Errorf(
			"schema %s@%s is not stored and no spec was supplied",
			schema.Name, schema.Version)
	} else if err != nil {
		return cs, twirp.InternalErrorf("get stored schema: %v", err)
	}

	err = json.Unmarshal(stored.Spec, &cs)
	if err != nil {
		return cs, twirp.InternalErrorf(
			"decode stored schema %s@%s: %v",
			schema.Name, schema.Version, err)
	}

	return cs, nil
}

// ActivateConfigGeneration implements user.Configuration.
func (s *ConfigurationService) ActivateConfigGeneration(
	ctx context.Context, req *user.ActivateConfigGenerationRequest,
) (*user.ActivateConfigGenerationResponse, error) {
	_, err := elephantine.RequireAnyScope(ctx, ScopeSchemaAdmin)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	if req.Id < 1 {
		return nil, twirp.RequiredArgumentError("id")
	}

	gen, err := s.store.ActivateConfigGeneration(ctx, req.Id)
	if errors.Is(err, ErrGenerationNotFound) {
		return nil, twirp.NotFoundError("no such generation")
	} else if err != nil {
		return nil, twirp.InternalErrorf("activate generation: %v", err)
	}

	return &user.ActivateConfigGenerationResponse{
		Generation: configGenerationToRPC(gen),
	}, nil
}

// GetActiveConfigGeneration implements user.Configuration.
func (s *ConfigurationService) GetActiveConfigGeneration(
	ctx context.Context, req *user.GetActiveConfigGenerationRequest,
) (*user.GetActiveConfigGenerationResponse, error) {
	_, err := elephantine.RequireAnyScope(ctx,
		ScopeSchemaAdmin, ScopeSchemaRead)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	changed, err := s.waitForGenerationChange(
		ctx, req.KnownId, req.WaitSeconds)
	if err != nil {
		return nil, twirp.InternalErrorf(
			"wait for generation change: %v", err)
	}

	if !changed && req.OnlyChanged {
		return &user.GetActiveConfigGenerationResponse{
			Unchanged: true,
		}, nil
	}

	gen, err := s.store.GetActiveConfigGeneration(ctx)
	if err != nil {
		return nil, twirp.InternalErrorf("get active generation: %v", err)
	}

	if gen == nil {
		return &user.GetActiveConfigGenerationResponse{}, nil
	}

	return &user.GetActiveConfigGenerationResponse{
		Generation: configGenerationToRPC(gen),
	}, nil
}

func (s *ConfigurationService) waitForGenerationChange(
	ctx context.Context, knownID int64, waitSeconds int64,
) (bool, error) {
	if waitSeconds <= 0 || waitSeconds > 10 {
		waitSeconds = 10
	}

	timeout := time.Duration(waitSeconds) * time.Second

	ch := make(chan SchemaEvent, 1)

	s.store.OnSchemaUpdate(ctx, ch)

	for {
		currentID, err := s.store.GetActiveConfigGenerationID(ctx)
		if err != nil {
			return false, fmt.Errorf(
				"read active generation id: %w", err)
		}

		if currentID != knownID {
			return true, nil
		}

		select {
		case <-ch:
		case <-time.After(timeout):
			return false, nil
		case <-ctx.Done():
			return false, twirp.Canceled.Error("context cancelled")
		}
	}
}

// ListConfigGenerations implements user.Configuration.
func (s *ConfigurationService) ListConfigGenerations(
	ctx context.Context, req *user.ListConfigGenerationsRequest,
) (*user.ListConfigGenerationsResponse, error) {
	_, err := elephantine.RequireAnyScope(ctx,
		ScopeSchemaAdmin, ScopeSchemaRead)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	pageSize := req.PageSize

	if pageSize <= 0 {
		pageSize = 50
	}

	if pageSize > 200 {
		pageSize = 200
	}

	generations, err := s.store.ListConfigGenerations(ctx, req.Before, pageSize)
	if err != nil {
		return nil, twirp.InternalErrorf("list generations: %v", err)
	}

	res := user.ListConfigGenerationsResponse{
		Generations: make([]*user.ConfigGeneration, len(generations)),
	}

	for i, gen := range generations {
		res.Generations[i] = configGenerationToRPC(gen)
	}

	return &res, nil
}

// GetSchema implements user.Configuration.
func (s *ConfigurationService) GetSchema(
	ctx context.Context, req *user.GetSchemaRequest,
) (*user.GetSchemaResponse, error) {
	_, err := elephantine.RequireAnyScope(ctx,
		ScopeSchemaAdmin, ScopeSchemaRead)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	if req.Name == "" {
		return nil, twirp.RequiredArgumentError("name")
	}

	schema, err := s.store.GetSchema(ctx, req.Name, req.Version)
	if errors.Is(err, ErrSchemaNotFound) {
		return nil, twirp.NotFoundError("no such schema")
	} else if err != nil {
		return nil, twirp.InternalErrorf("get schema: %v", err)
	}

	return &user.GetSchemaResponse{
		Version: schema.Version,
		Spec:    string(schema.Spec),
		Usage:   schemaUsageToRPC(schema.Usage),
	}, nil
}

// GetDeprecations implements user.Configuration.
func (s *ConfigurationService) GetDeprecations(
	ctx context.Context, _ *user.GetDeprecationsRequest,
) (*user.GetDeprecationsResponse, error) {
	_, err := elephantine.RequireAnyScope(ctx,
		ScopeSchemaAdmin, ScopeSchemaRead)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	deprecations, err := s.store.GetDeprecations(ctx)
	if err != nil {
		return nil, twirp.InternalErrorf("list deprecations: %v", err)
	}

	res := user.GetDeprecationsResponse{
		Deprecations: make([]*user.Deprecation, len(deprecations)),
	}

	for i, dep := range deprecations {
		res.Deprecations[i] = &user.Deprecation{
			Label:    dep.Label,
			Enforced: dep.Enforced,
		}
	}

	return &res, nil
}

// UpdateDeprecation implements user.Configuration.
func (s *ConfigurationService) UpdateDeprecation(
	ctx context.Context, req *user.UpdateDeprecationRequest,
) (*user.UpdateDeprecationResponse, error) {
	_, err := elephantine.RequireAnyScope(ctx, ScopeSchemaAdmin)
	if err != nil {
		return nil, err //nolint:wrapcheck
	}

	if req.Deprecation == nil {
		return nil, twirp.RequiredArgumentError("deprecation")
	}

	if req.Deprecation.Label == "" {
		return nil, twirp.RequiredArgumentError("deprecation.label")
	}

	err = s.store.UpdateDeprecation(ctx, Deprecation{
		Label:    req.Deprecation.Label,
		Enforced: req.Deprecation.Enforced,
	})
	if err != nil {
		return nil, twirp.InternalErrorf("update deprecation: %v", err)
	}

	return &user.UpdateDeprecationResponse{}, nil
}

func configGenerationToRPC(gen *ConfigGeneration) *user.ConfigGeneration {
	res := user.ConfigGeneration{
		Id:          gen.ID,
		Description: gen.Description,
		Active:      gen.Active,
		Created:     gen.Created.Format(time.RFC3339),
		Schemas: make(
			[]*user.ConfigGenerationSchema, len(gen.Schemas)),
	}

	if gen.Activated != nil {
		res.Activated = gen.Activated.Format(time.RFC3339)
	}

	for i, schema := range gen.Schemas {
		res.Schemas[i] = &user.ConfigGenerationSchema{
			Name:    schema.Name,
			Version: schema.Version,
			Spec:    string(schema.Spec),
			Usage:   schemaUsageToRPC(schema.Usage),
		}
	}

	return &res
}

func schemaUsageFromRPC(usage user.SchemaUsage) (postgres.SchemaUsage, error) {
	switch usage {
	case user.SchemaUsage_SCHEMA_USAGE_SETTINGS:
		return postgres.SchemaUsageSettings, nil
	case user.SchemaUsage_SCHEMA_USAGE_MESSAGES:
		return postgres.SchemaUsageMessages, nil
	case user.SchemaUsage_SCHEMA_USAGE_UNSPECIFIED,
		user.SchemaUsage_SCHEMA_USAGE_EDITORIAL,
		user.SchemaUsage_SCHEMA_USAGE_DISTRIBUTION:
		return "", fmt.Errorf(
			"usage must be one of %q or %q",
			user.SchemaUsage_SCHEMA_USAGE_SETTINGS,
			user.SchemaUsage_SCHEMA_USAGE_MESSAGES)
	default:
		return "", fmt.Errorf("unknown usage %d", usage)
	}
}

func schemaUsageToRPC(usage postgres.SchemaUsage) user.SchemaUsage {
	switch usage {
	case postgres.SchemaUsageSettings:
		return user.SchemaUsage_SCHEMA_USAGE_SETTINGS
	case postgres.SchemaUsageMessages:
		return user.SchemaUsage_SCHEMA_USAGE_MESSAGES
	default:
		return user.SchemaUsage_SCHEMA_USAGE_UNSPECIFIED
	}
}
