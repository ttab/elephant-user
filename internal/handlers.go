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

const (
	// pollDefaultSize is how many items a poll or list returns when the
	// request does not say.
	pollDefaultSize = 10
	// pollMaxSize caps what a poll or list returns in one response. A
	// request over the cap is served the cap, as the repository's
	// eventlog does.
	pollMaxSize = 100
)

// clampSize applies the default and the ceiling to a requested page size.
func clampSize(size int64) int64 {
	if size <= 0 {
		return pollDefaultSize
	}

	return min(size, pollMaxSize)
}
