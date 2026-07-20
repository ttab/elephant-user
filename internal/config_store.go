package internal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/ttab/elephant-user/postgres"
	"github.com/ttab/elephantine/pg"
)

// Interface guards.
var (
	_ ConfigurationStore = &PGStore{}
	_ ValidatorStore     = &PGStore{}
)

// RegisterConfigGeneration registers a config generation. Registration
// is idempotent: a generation containing the same set of schemas
// returns the already registered generation. If activation is requested
// for an existing inactive generation it will be activated.
func (s *PGStore) RegisterConfigGeneration(
	ctx context.Context, description string,
	schemas []ConfigSchema, activate bool,
) (*ConfigGeneration, error) {
	hash := configGenerationIdentityHash(schemas)

	var (
		genID   int64
		hashErr error
	)

	// Retry on identity hash conflict, which can happen if another
	// instance registers the same generation concurrently. The second
	// attempt will find the existing generation by hash.
	for attempt := 0; attempt < 2; attempt++ {
		id, err := s.registerConfigGeneration(
			ctx, description, schemas, activate, hash)
		if err != nil && pg.IsConstraintError(
			err, "config_generation_identity_hash_key") {
			hashErr = err

			continue
		} else if err != nil {
			return nil, err
		}

		genID = id
		hashErr = nil

		break
	}

	if hashErr != nil {
		return nil, fmt.Errorf(
			"register generation after retry: %w", hashErr)
	}

	return s.GetConfigGeneration(ctx, genID)
}

func (s *PGStore) registerConfigGeneration(
	ctx context.Context, description string,
	schemas []ConfigSchema, activate bool, hash string,
) (_ int64, outErr error) {
	tx, err := s.dbpool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}

	defer pg.Rollback(tx, &outErr)

	q := postgres.New(tx)

	err = ensureConfigSchemas(ctx, q, schemas)
	if err != nil {
		return 0, err
	}

	existing, err := q.GetConfigGenerationByIdentityHash(ctx, hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("get generation by identity hash: %w", err)
	}

	var (
		genID  int64
		active bool
	)

	if err == nil {
		genID = existing.ID
		active = existing.Active
	} else {
		row, err := q.InsertConfigGeneration(ctx,
			postgres.InsertConfigGenerationParams{
				IdentityHash: hash,
				Description:  description,
			})
		if err != nil {
			return 0, fmt.Errorf("insert generation: %w", err)
		}

		for i, schema := range schemas {
			err = q.InsertConfigGenerationSchema(ctx,
				postgres.InsertConfigGenerationSchemaParams{
					GenerationID: row.ID,
					Name:         schema.Name,
					Version:      schema.Version,
					Ordinal:      int32(i),
				})
			if err != nil {
				return 0, fmt.Errorf(
					"insert generation schema %q: %w",
					schema.Name, err)
			}
		}

		genID = row.ID
		active = row.Active
	}

	// Only notify when the active generation changes: registering an
	// inactive generation doesn't affect the active schema set that
	// long-polls and the validator care about.
	if activate && !active {
		err = activateConfigGeneration(ctx, q, genID)
		if err != nil {
			return 0, err
		}

		err = notifySchemasUpdated(ctx, q, SchemaEvent{Type: "activated"})
		if err != nil {
			return 0, fmt.Errorf("send notification: %w", err)
		}
	}

	err = tx.Commit(ctx)
	if err != nil {
		return 0, fmt.Errorf("commit transaction: %w", err)
	}

	return genID, nil
}

// ensureConfigSchemas stores the supplied schemas. Schemas that already
// exist must match the stored spec and usage, schemas that don't exist
// must have a spec.
func ensureConfigSchemas(
	ctx context.Context, q *postgres.Queries, schemas []ConfigSchema,
) error {
	for _, schema := range schemas {
		stored, err := q.GetSchema(ctx, postgres.GetSchemaParams{
			Name:    schema.Name,
			Version: schema.Version,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			if len(schema.Spec) == 0 {
				return fmt.Errorf("schema %s@%s: %w",
					schema.Name, schema.Version,
					ErrSchemaSpecMissing)
			}

			err = q.EnsureSchema(ctx, postgres.EnsureSchemaParams{
				Name:    schema.Name,
				Version: schema.Version,
				Spec:    schema.Spec,
				Usage:   schema.Usage,
			})
			if err != nil {
				return fmt.Errorf("store schema %s@%s: %w",
					schema.Name, schema.Version, err)
			}

			continue
		} else if err != nil {
			return fmt.Errorf("get schema %s@%s: %w",
				schema.Name, schema.Version, err)
		}

		if stored.Usage != schema.Usage {
			return fmt.Errorf("schema %s@%s: %w",
				schema.Name, schema.Version, ErrSchemaMismatch)
		}

		if len(schema.Spec) == 0 {
			continue
		}

		match, err := jsonEqual(schema.Spec, stored.Spec)
		if err != nil {
			return fmt.Errorf("compare schema %s@%s specs: %w",
				schema.Name, schema.Version, err)
		}

		if !match {
			return fmt.Errorf("schema %s@%s: %w",
				schema.Name, schema.Version, ErrSchemaMismatch)
		}
	}

	return nil
}

// ActivateConfigGeneration activates a registered config generation,
// deactivating the previously active generation.
func (s *PGStore) ActivateConfigGeneration(
	ctx context.Context, id int64,
) (*ConfigGeneration, error) {
	err := s.activateGenerationTx(ctx, id)
	if err != nil {
		return nil, err
	}

	return s.GetConfigGeneration(ctx, id)
}

func (s *PGStore) activateGenerationTx(
	ctx context.Context, id int64,
) (outErr error) {
	tx, err := s.dbpool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer pg.Rollback(tx, &outErr)

	q := postgres.New(tx)

	gen, err := q.GetConfigGeneration(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrGenerationNotFound
	} else if err != nil {
		return fmt.Errorf("get generation: %w", err)
	}

	if !gen.Active {
		err = activateConfigGeneration(ctx, q, id)
		if err != nil {
			return err
		}

		err = notifySchemasUpdated(ctx, q, SchemaEvent{Type: "activated"})
		if err != nil {
			return fmt.Errorf("send notification: %w", err)
		}
	}

	err = tx.Commit(ctx)
	if err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

func activateConfigGeneration(
	ctx context.Context, q *postgres.Queries, id int64,
) error {
	err := q.DeactivateActiveConfigGeneration(ctx, id)
	if err != nil {
		return fmt.Errorf("deactivate current generation: %w", err)
	}

	err = q.ActivateConfigGeneration(ctx, id)
	if err != nil {
		return fmt.Errorf("activate generation: %w", err)
	}

	return nil
}

// GetActiveConfigGeneration returns the active config generation with schema
// specs populated, or nil if no generation is active.
func (s *PGStore) GetActiveConfigGeneration(
	ctx context.Context,
) (*ConfigGeneration, error) {
	row, err := s.q.GetActiveConfigGeneration(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		//nolint:nilnil
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("get active generation: %w", err)
	}

	// Fetch the schemas by the generation ID we just read, so that a
	// concurrent activation cannot give us the schemas of another
	// generation.
	rows, err := s.q.GetConfigGenerationSchemasWithSpec(ctx, row.ID)
	if err != nil {
		return nil, fmt.Errorf("get generation schemas: %w", err)
	}

	gen := configGenerationFromRow(
		row.ID, row.Description, row.Active,
		row.CreatedAt, row.ActivatedAt)

	gen.Schemas = make([]ConfigSchema, len(rows))

	for i, r := range rows {
		gen.Schemas[i] = ConfigSchema{
			Name:    r.Name,
			Version: r.Version,
			Spec:    r.Spec,
			Usage:   r.Usage,
		}
	}

	return gen, nil
}

// GetActiveConfigGenerationID returns the ID of the active config generation,
// or zero if no generation is active.
func (s *PGStore) GetActiveConfigGenerationID(ctx context.Context) (int64, error) {
	row, err := s.q.GetActiveConfigGeneration(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	} else if err != nil {
		return 0, fmt.Errorf("get active generation: %w", err)
	}

	return row.ID, nil
}

// GetActiveSchemas returns the schemas of the active config
// generation with specs populated, in registration order.
func (s *PGStore) GetActiveSchemas(
	ctx context.Context,
) ([]ConfigSchema, error) {
	rows, err := s.q.GetActiveSchemas(ctx)
	if err != nil {
		return nil, fmt.Errorf("get active generation schemas: %w", err)
	}

	schemas := make([]ConfigSchema, len(rows))

	for i, row := range rows {
		schemas[i] = ConfigSchema{
			Name:    row.Name,
			Version: row.Version,
			Spec:    row.Spec,
			Usage:   row.Usage,
		}
	}

	return schemas, nil
}

// GetConfigGeneration returns a config generation with schema references
// (without specs).
func (s *PGStore) GetConfigGeneration(
	ctx context.Context, id int64,
) (*ConfigGeneration, error) {
	row, err := s.q.GetConfigGeneration(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrGenerationNotFound
	} else if err != nil {
		return nil, fmt.Errorf("get generation: %w", err)
	}

	gen := configGenerationFromRow(
		row.ID, row.Description, row.Active,
		row.CreatedAt, row.ActivatedAt)

	err = s.addGenerationSchemaRefs(ctx, gen)
	if err != nil {
		return nil, err
	}

	return gen, nil
}

// ListConfigGenerations returns config generations with schema references
// (without specs), newest first.
func (s *PGStore) ListConfigGenerations(
	ctx context.Context, before int64, pageSize int64,
) ([]*ConfigGeneration, error) {
	rows, err := s.q.ListConfigGenerations(ctx,
		postgres.ListConfigGenerationsParams{
			Before:   before,
			PageSize: pageSize,
		})
	if err != nil {
		return nil, fmt.Errorf("list generations: %w", err)
	}

	generations := make([]*ConfigGeneration, len(rows))

	for i, row := range rows {
		gen := configGenerationFromRow(
			row.ID, row.Description, row.Active,
			row.CreatedAt, row.ActivatedAt)

		err = s.addGenerationSchemaRefs(ctx, gen)
		if err != nil {
			return nil, err
		}

		generations[i] = gen
	}

	return generations, nil
}

func (s *PGStore) addGenerationSchemaRefs(
	ctx context.Context, gen *ConfigGeneration,
) error {
	refs, err := s.q.GetConfigGenerationSchemas(ctx, gen.ID)
	if err != nil {
		return fmt.Errorf("get generation schemas: %w", err)
	}

	gen.Schemas = make([]ConfigSchema, len(refs))

	for i, ref := range refs {
		gen.Schemas[i] = ConfigSchema{
			Name:    ref.Name,
			Version: ref.Version,
			Usage:   ref.Usage,
		}
	}

	return nil
}

// GetSchema returns a stored schema. An empty version returns the
// version from the active generation.
func (s *PGStore) GetSchema(
	ctx context.Context, name string, version string,
) (*ConfigSchema, error) {
	var (
		schema ConfigSchema
		err    error
	)

	if version == "" {
		row, e := s.q.GetActiveSchema(ctx, name)
		schema = ConfigSchema{
			Name:    row.Name,
			Version: row.Version,
			Spec:    row.Spec,
			Usage:   row.Usage,
		}
		err = e
	} else {
		row, e := s.q.GetSchema(ctx, postgres.GetSchemaParams{
			Name:    name,
			Version: version,
		})
		schema = ConfigSchema{
			Name:    row.Name,
			Version: row.Version,
			Spec:    row.Spec,
			Usage:   row.Usage,
		}
		err = e
	}

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSchemaNotFound
	} else if err != nil {
		return nil, fmt.Errorf("get schema: %w", err)
	}

	return &schema, nil
}

// GetDeprecations returns all deprecation statuses sorted by label.
func (s *PGStore) GetDeprecations(ctx context.Context) ([]Deprecation, error) {
	rows, err := s.q.GetDeprecations(ctx)
	if err != nil {
		return nil, fmt.Errorf("list deprecations: %w", err)
	}

	deprecations := make([]Deprecation, len(rows))

	for i, row := range rows {
		deprecations[i] = Deprecation{
			Label:    row.Label,
			Enforced: row.Enforced,
		}
	}

	return deprecations, nil
}

// UpdateDeprecation creates or updates a deprecation status.
func (s *PGStore) UpdateDeprecation(
	ctx context.Context, deprecation Deprecation,
) (outErr error) {
	tx, err := s.dbpool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer pg.Rollback(tx, &outErr)

	q := postgres.New(tx)

	err = q.UpsertDeprecation(ctx, postgres.UpsertDeprecationParams{
		Label:    deprecation.Label,
		Enforced: deprecation.Enforced,
	})
	if err != nil {
		return fmt.Errorf("upsert deprecation: %w", err)
	}

	err = pgNotify(ctx, q, NotifyChannelDeprecationUpdate, DeprecationEvent{
		Label: deprecation.Label,
	})
	if err != nil {
		return fmt.Errorf("send notification: %w", err)
	}

	err = tx.Commit(ctx)
	if err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

// GetEnforcedDeprecations returns the labels of all enforced
// deprecations.
func (s *PGStore) GetEnforcedDeprecations(
	ctx context.Context,
) (map[string]bool, error) {
	labels, err := s.q.GetEnforcedDeprecations(ctx)
	if err != nil {
		return nil, fmt.Errorf("get enforced deprecations: %w", err)
	}

	enforced := make(map[string]bool, len(labels))

	for _, label := range labels {
		enforced[label] = true
	}

	return enforced, nil
}

// OnSchemaUpdate notifies the channel ch of config generation changes.
// Subscription is automatically cancelled once the context is
// cancelled.
//
// Note that we don't provide any delivery guarantees for these events.
// non-blocking send is used on ch, so if it's unbuffered events will be
// discarded if the receiver is busy.
func (s *PGStore) OnSchemaUpdate(
	ctx context.Context, ch chan SchemaEvent,
) {
	go s.Schemas.ListenAll(ctx, ch)
}

// OnDeprecationUpdate notifies the channel ch of deprecation changes.
// Subscription is automatically cancelled once the context is
// cancelled.
//
// Note that we don't provide any delivery guarantees for these events.
// non-blocking send is used on ch, so if it's unbuffered events will be
// discarded if the receiver is busy.
func (s *PGStore) OnDeprecationUpdate(
	ctx context.Context, ch chan DeprecationEvent,
) {
	go s.Deprecations.ListenAll(ctx, ch)
}

func notifySchemasUpdated(
	ctx context.Context, q *postgres.Queries, payload SchemaEvent,
) error {
	return pgNotify(ctx, q, NotifyChannelSchemaUpdate, payload)
}

func configGenerationFromRow(
	id int64, description string, active bool,
	created pgtype.Timestamptz, activated pgtype.Timestamptz,
) *ConfigGeneration {
	gen := ConfigGeneration{
		ID:          id,
		Description: description,
		Active:      active,
		Created:     created.Time,
	}

	if activated.Valid {
		t := activated.Time

		gen.Activated = &t
	}

	return &gen
}

// configGenerationIdentityHash computes an identity hash for the schema
// set of a generation, invariant to the order of the schemas.
func configGenerationIdentityHash(schemas []ConfigSchema) string {
	parts := make([]string, len(schemas))

	for i, schema := range schemas {
		parts[i] = fmt.Sprintf("schema:%s@%s", schema.Name, schema.Version)
	}

	slices.Sort(parts)

	h := sha256.New()

	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}

	return hex.EncodeToString(h.Sum(nil))
}

// jsonEqual compares two JSON documents structurally.
func jsonEqual(a []byte, b []byte) (bool, error) {
	var av, bv any

	err := json.Unmarshal(a, &av)
	if err != nil {
		return false, fmt.Errorf("unmarshal first document: %w", err)
	}

	err = json.Unmarshal(b, &bv)
	if err != nil {
		return false, fmt.Errorf("unmarshal second document: %w", err)
	}

	ac, err := json.Marshal(av)
	if err != nil {
		return false, fmt.Errorf("marshal first document: %w", err)
	}

	bc, err := json.Marshal(bv)
	if err != nil {
		return false, fmt.Errorf("marshal second document: %w", err)
	}

	return string(ac) == string(bc), nil
}
