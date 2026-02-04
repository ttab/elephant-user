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

func NewValidator(_ context.Context) (*Validator, error) {
	var local revisor.ConstraintSet

	dec := json.NewDecoder(bytes.NewReader(schemaMessages))

	dec.DisallowUnknownFields()

	err := dec.Decode(&local)
	if err != nil {
		return nil, fmt.Errorf("unmarshal JSON: %w", err)
	}

	validator, err := revisor.NewValidator(local)
	if err != nil {
		return nil, fmt.Errorf("create validator: %w", err)
	}

	return &Validator{
		validator: validator,
	}, nil
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
