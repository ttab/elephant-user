package internal

import (
	"strings"
	"testing"
)

func TestCheckReverseDomainNotation(t *testing.T) {
	// Long is fine: the rule bounds the shape, not the length.
	label := "a" + strings.Repeat("b", 62)
	long := label + "." + label + "." + label

	cases := []struct {
		Name  string
		Value string
		Valid bool
	}{
		{Name: "ours", Value: "se.ecms.user.comments.factcheck", Valid: true},
		{Name: "three labels", Value: "com.example.review", Valid: true},
		{Name: "hyphens and digits", Value: "com.example-2.fact-check9", Valid: true},
		{Name: "long labels", Value: long, Valid: true},
		{Name: "bare word", Value: "factcheck"},
		{Name: "two labels", Value: "example.factcheck"},
		{Name: "upper case", Value: "se.ecms.user.comments.FactCheck"},
		{Name: "label starting with a digit", Value: "se.ecms.2user.factcheck"},
		{Name: "label starting with a hyphen", Value: "se.ecms.-user.factcheck"},
		{Name: "empty label", Value: "se..ecms.factcheck"},
		{Name: "leading dot", Value: ".se.ecms.factcheck"},
		{Name: "trailing dot", Value: "se.ecms.factcheck."},
		{Name: "underscore", Value: "se.ecms.fact_check"},
		{Name: "slash", Value: "se.ecms/user.factcheck"},
		{Name: "space", Value: "se.ecms.fact check"},
		{Name: "non-ascii", Value: "se.ecms.faktagranskningå"},
		{Name: "trailing newline", Value: "se.ecms.factcheck\n"},
		{Name: "empty", Value: ""},
	}

	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			err := checkReverseDomainNotation(c.Value)

			switch {
			case c.Valid && err != nil:
				t.Fatalf("expected %q to be accepted, got %v", c.Value, err)
			case !c.Valid && err == nil:
				t.Fatalf("expected %q to be refused", c.Value)
			}
		})
	}
}
