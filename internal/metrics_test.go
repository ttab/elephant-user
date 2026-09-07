package internal_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ttab/elephant-user/internal"
	"github.com/ttab/elephantine/test"
)

// TestMetricsLint registers the full service metric set against a fresh
// registry and runs the Prometheus linter over it, so that naming and
// help-text violations fail the build.
func TestMetricsLint(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()

	m, err := internal.NewMetrics(reg)
	test.Mustf(t, err, "register metrics")

	// Vectors only surface once they have a labelled child.
	m.Deprecations.WithLabelValues("example").Inc()
	m.DocsWithDeprecations.WithLabelValues("core/example").Inc()

	problems, err := testutil.GatherAndLint(reg)
	test.Mustf(t, err, "gather and lint metrics")

	for _, p := range problems {
		t.Errorf("%s: %s", p.Metric, p.Text)
	}
}
