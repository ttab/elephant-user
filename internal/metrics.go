package internal

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephantine"
)

// Metric label names.
const (
	LabelDeprecation = "label"
	LabelDocType     = "doc_type"
	LabelOutcome     = "outcome"
)

// Outcomes of a document permission lookup.
const (
	// PermissionLookupHit is an answer served from the cache.
	PermissionLookupHit = "hit"
	// PermissionLookupMiss is an answer the repository gave.
	PermissionLookupMiss = "miss"
	// PermissionLookupDenied is a token the repository refused outright.
	PermissionLookupDenied = "denied"
	// PermissionLookupNotFound is a document the repository does not know.
	PermissionLookupNotFound = "not_found"
	// PermissionLookupError is a lookup that failed.
	PermissionLookupError = "error"
)

// Metrics holds every Prometheus collector the service owns. It is
// constructed once with NewMetrics and handed to the subsystems that
// report through it.
type Metrics struct {
	// Deprecations counts uses of deprecated schema constructs by
	// deprecation label.
	Deprecations *prometheus.CounterVec
	// DocsWithDeprecations counts validated documents that used at least
	// one deprecated construct, by document type.
	DocsWithDeprecations *prometheus.CounterVec
	// PermissionLookups counts document permission lookups against the
	// repository by outcome.
	PermissionLookups *prometheus.CounterVec
}

// NewMetrics registers the service metrics with reg and fails if any
// registration clashes.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}

	h := elephantine.NewMetricsHelper(reg)

	var m Metrics

	h.CounterVec(&m.Deprecations, prometheus.CounterOpts{
		Name: "elephant_user_deprecations_total",
		Help: "Uses of deprecated schema constructs by deprecation label. " +
			"A label that stops counting can have its deprecation enforced.",
	}, []string{LabelDeprecation})

	h.CounterVec(&m.DocsWithDeprecations, prometheus.CounterOpts{
		Name: "elephant_user_docs_with_deprecations_total",
		Help: "Validated documents that used at least one deprecated " +
			"construct, by document type.",
	}, []string{LabelDocType})

	h.CounterVec(&m.PermissionLookups, prometheus.CounterOpts{
		Name: "elephant_user_permission_lookups_total",
		Help: "Document permission lookups by outcome: hit from the cache, " +
			"miss answered by the repository, denied for a token the " +
			"repository refused, not_found, or error. A rising error " +
			"rate means document access checks are failing.",
	}, []string{LabelOutcome})

	err := h.Err()
	if err != nil {
		return nil, fmt.Errorf("register service metrics: %w", err)
	}

	return &m, nil
}
