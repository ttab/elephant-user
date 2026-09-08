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

	err := h.Err()
	if err != nil {
		return nil, fmt.Errorf("register service metrics: %w", err)
	}

	return &m, nil
}
