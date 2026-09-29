package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"connectrpc.com/connect"
	newsdoc_rpc "github.com/ttab/elephant-api/newsdoc"
	"github.com/ttab/elephant-user/postgres"
	"github.com/ttab/elephantine/rpc"
	"github.com/ttab/newsdoc"
	"github.com/ttab/revisor"
)

// waitEndedError maps the end of a long-poll wait to the RPC code the
// caller expects: deadline_exceeded when the deadline they set ran out,
// canceled when they went away.
func waitEndedError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return rpc.Errorf(connect.CodeDeadlineExceeded,
			"the deadline for the call was exceeded")
	}

	return rpc.Errorf(connect.CodeCanceled, "context cancelled")
}

type DocumentValidator interface {
	ValidateDocument(
		ctx context.Context, usage postgres.SchemaUsage,
		document *newsdoc.Document,
	) ([]revisor.ValidationResult, error)
}

// validationError builds the invalid_argument error for a document that
// failed validation: the first result in the message, the count and every
// result as metadata.
func validationError(validationResult []revisor.ValidationResult) error {
	err := rpc.Errorf(connect.CodeInvalidArgument,
		"the document had %d validation errors, the first one is: %v",
		len(validationResult), validationResult[0].String())

	err = rpc.WithMeta(err, "err_count",
		strconv.Itoa(len(validationResult)))

	for i := range validationResult {
		err = rpc.WithMeta(err, strconv.Itoa(i),
			validationResult[i].String())
	}

	return err
}

// unmarshalDocument decodes a stored NewsDoc payload into the RPC document.
func unmarshalDocument(raw json.RawMessage) (*newsdoc_rpc.Document, error) {
	var doc newsdoc_rpc.Document

	err := json.Unmarshal(raw, &doc)
	if err != nil {
		return nil, fmt.Errorf("unmarshal payload: %w", err)
	}

	return &doc, nil
}
