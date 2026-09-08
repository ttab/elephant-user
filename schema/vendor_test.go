package schema_test

import (
	"testing"

	"github.com/ttab/mage/libschema"
)

// TestLibraryMigrationsAreCovered fails when a library named in vendor.json
// has a migration that this service's schema doesn't create. Neither
// `mage sql:migrate` nor elephant-platform's `setup db migrate` looks inside a
// dependency, so a library that adds a table would otherwise build, test and
// deploy before failing at runtime on a table nobody created.
func TestLibraryMigrationsAreCovered(t *testing.T) {
	problems, err := libschema.Check(".")
	if err != nil {
		t.Fatalf("check vendored migrations: %v", err)
	}

	for _, p := range problems {
		t.Errorf("%s", p)
	}
}
