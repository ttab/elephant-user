package internal

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/ttab/newsdoc"
	"github.com/ttab/revisor"
)

//go:embed schema_messages.json
var schemaMessages []byte

//go:embed schema_settings.json
var schemaSettings []byte

func NewValidator(_ context.Context) (*Validator, error) {
	messages, err := decodeConstraintSet(schemaMessages, "schema_messages.json")
	if err != nil {
		return nil, err
	}

	settings, err := decodeConstraintSet(schemaSettings, "schema_settings.json")
	if err != nil {
		return nil, err
	}

	v, err := revisor.NewValidator(messages, settings)
	if err != nil {
		return nil, fmt.Errorf("create validator: %w", err)
	}

	return &Validator{validator: v}, nil
}

type Validator struct {
	validator *revisor.Validator
}

func (v *Validator) ValidateDocument(
	ctx context.Context, doc *newsdoc.Document,
) ([]revisor.ValidationResult, error) {
	res, err := v.validator.ValidateDocument(ctx, doc)
	if err != nil {
		return nil, fmt.Errorf("validate document: %w", err)
	}

	return res, nil
}

func decodeConstraintSet(data []byte, name string) (revisor.ConstraintSet, error) {
	var cs revisor.ConstraintSet

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	err := dec.Decode(&cs)
	if err != nil {
		return cs, fmt.Errorf("decode %s: %w", name, err)
	}

	return cs, nil
}
