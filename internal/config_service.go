package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephant-user/postgres"
	"github.com/ttab/elephantine/rpc"
	"github.com/ttab/revisor"
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
	_, err := rpc.RequireAnyScope(ctx, ScopeSchemaAdmin)
	if err != nil {
		return nil, err
	}

	if len(req.Schemas) == 0 {
		return nil, rpc.RequiredArgument("schemas")
	}

	schemas := make([]ConfigSchema, len(req.Schemas))
	seen := make(map[string]bool, len(req.Schemas))

	// Constraint sets grouped by usage for the validator dry-run.
	grouped := make(map[postgres.SchemaUsage][]revisor.ConstraintSet)

	for i, schema := range req.Schemas {
		if schema.Name == "" {
			return nil, rpc.RequiredArgument(
				fmt.Sprintf("schemas.%d.name", i))
		}

		if schema.Version == "" {
			return nil, rpc.RequiredArgument(
				fmt.Sprintf("schemas.%d.version", i))
		}

		if seen[schema.Name] {
			return nil, rpc.InvalidArgumentf(
				fmt.Sprintf("schemas.%d.name", i),
				"%q is listed twice", schema.Name)
		}

		seen[schema.Name] = true

		usage, err := schemaUsageFromRPC(schema.Usage)
		if err != nil {
			return nil, rpc.InvalidArgumentf(
				fmt.Sprintf("schemas.%d.usage", i),
				"of %s@%s: %w", schema.Name, schema.Version, err)
		}

		cs, err := s.resolveConstraintSet(ctx, i, schema)
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
			return nil, rpc.Errorf(connect.CodeInvalidArgument,
				"the schemas for usage %q cannot form a valid constraint set: %w",
				usage, err)
		}
	}

	gen, err := s.store.RegisterConfigGeneration(
		ctx, req.Description, schemas, req.Activate)
	if errors.Is(err, ErrSchemaMismatch) || errors.Is(err, ErrSchemaSpecMissing) {
		return nil, rpc.Errorf(connect.CodeInvalidArgument, "%w", err)
	} else if err != nil {
		return nil, rpc.Internalf("register generation: %w", err)
	}

	return &user.RegisterConfigGenerationResponse{
		Generation: configGenerationToRPC(gen),
	}, nil
}

// resolveConstraintSet decodes the supplied schema spec, or loads the
// stored spec when none is supplied. The index names the request field
// in argument errors.
func (s *ConfigurationService) resolveConstraintSet(
	ctx context.Context, i int, schema *user.ConfigGenerationSchema,
) (revisor.ConstraintSet, error) {
	var cs revisor.ConstraintSet

	argument := fmt.Sprintf("schemas.%d.spec", i)

	if schema.Spec != "" {
		dec := json.NewDecoder(bytes.NewReader([]byte(schema.Spec)))

		dec.DisallowUnknownFields()

		err := dec.Decode(&cs)
		if err != nil {
			return cs, rpc.InvalidArgumentf(argument,
				"of %s@%s is not a valid constraint set: %w",
				schema.Name, schema.Version, err)
		}

		return cs, nil
	}

	stored, err := s.store.GetSchema(ctx, schema.Name, schema.Version)
	if errors.Is(err, ErrSchemaNotFound) {
		return cs, rpc.InvalidArgumentf(argument,
			"is required, %s@%s is not stored",
			schema.Name, schema.Version)
	} else if err != nil {
		return cs, rpc.Internalf("get stored schema: %w", err)
	}

	err = json.Unmarshal(stored.Spec, &cs)
	if err != nil {
		return cs, rpc.Internalf(
			"decode stored schema %s@%s: %w",
			schema.Name, schema.Version, err)
	}

	return cs, nil
}

// ActivateConfigGeneration implements user.Configuration.
func (s *ConfigurationService) ActivateConfigGeneration(
	ctx context.Context, req *user.ActivateConfigGenerationRequest,
) (*user.ActivateConfigGenerationResponse, error) {
	_, err := rpc.RequireAnyScope(ctx, ScopeSchemaAdmin)
	if err != nil {
		return nil, err
	}

	if req.Id < 1 {
		return nil, rpc.RequiredArgument("id")
	}

	gen, err := s.store.ActivateConfigGeneration(ctx, req.Id)
	if errors.Is(err, ErrGenerationNotFound) {
		return nil, rpc.NotFound("no such generation")
	} else if err != nil {
		return nil, rpc.Internalf("activate generation: %w", err)
	}

	return &user.ActivateConfigGenerationResponse{
		Generation: configGenerationToRPC(gen),
	}, nil
}

// GetActiveConfigGeneration implements user.Configuration.
func (s *ConfigurationService) GetActiveConfigGeneration(
	ctx context.Context, req *user.GetActiveConfigGenerationRequest,
) (*user.GetActiveConfigGenerationResponse, error) {
	_, err := rpc.RequireAnyScope(ctx,
		ScopeSchemaAdmin, ScopeSchemaRead)
	if err != nil {
		return nil, err
	}

	changed, err := s.waitForGenerationChange(
		ctx, req.KnownId, req.WaitSeconds)
	if err != nil {
		if ctx.Err() != nil {
			return nil, waitEndedError(ctx)
		}

		return nil, rpc.Internalf(
			"wait for generation change: %w", err)
	}

	if !changed && req.OnlyChanged {
		return &user.GetActiveConfigGenerationResponse{
			Unchanged: true,
		}, nil
	}

	gen, err := s.store.GetActiveConfigGeneration(ctx)
	if err != nil {
		return nil, rpc.Internalf("get active generation: %w", err)
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
			return false, ctx.Err()
		}
	}
}

// ListConfigGenerations implements user.Configuration.
func (s *ConfigurationService) ListConfigGenerations(
	ctx context.Context, req *user.ListConfigGenerationsRequest,
) (*user.ListConfigGenerationsResponse, error) {
	_, err := rpc.RequireAnyScope(ctx,
		ScopeSchemaAdmin, ScopeSchemaRead)
	if err != nil {
		return nil, err
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
		return nil, rpc.Internalf("list generations: %w", err)
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
	_, err := rpc.RequireAnyScope(ctx,
		ScopeSchemaAdmin, ScopeSchemaRead)
	if err != nil {
		return nil, err
	}

	if req.Name == "" {
		return nil, rpc.RequiredArgument("name")
	}

	schema, err := s.store.GetSchema(ctx, req.Name, req.Version)
	if errors.Is(err, ErrSchemaNotFound) {
		return nil, rpc.NotFound("no such schema")
	} else if err != nil {
		return nil, rpc.Internalf("get schema: %w", err)
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
	_, err := rpc.RequireAnyScope(ctx,
		ScopeSchemaAdmin, ScopeSchemaRead)
	if err != nil {
		return nil, err
	}

	deprecations, err := s.store.GetDeprecations(ctx)
	if err != nil {
		return nil, rpc.Internalf("list deprecations: %w", err)
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
	_, err := rpc.RequireAnyScope(ctx, ScopeSchemaAdmin)
	if err != nil {
		return nil, err
	}

	if req.Deprecation == nil {
		return nil, rpc.RequiredArgument("deprecation")
	}

	if req.Deprecation.Label == "" {
		return nil, rpc.RequiredArgument("deprecation.label")
	}

	err = s.store.UpdateDeprecation(ctx, Deprecation{
		Label:    req.Deprecation.Label,
		Enforced: req.Deprecation.Enforced,
	})
	if err != nil {
		return nil, rpc.Internalf("update deprecation: %w", err)
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
