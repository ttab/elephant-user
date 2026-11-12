package internal

import (
	"strings"
	"testing"

	"github.com/ttab/newsdoc"
)

func TestCheckInlineLinkIDs(t *testing.T) {
	mention := newsdoc.Block{ID: "m1", Rel: "mention", URI: "core://user/anna"}
	seeAlso := newsdoc.Block{ID: "l1", Rel: "see-also", UUID: "9c1b8d0e-6f4a-4b8e-8a8f-0d2c3b4a5e6f"}

	cases := []struct {
		Name    string
		Text    string
		Links   []newsdoc.Block
		WantErr string
	}{
		{
			Name:  "bound mention and anchor",
			Text:  `Ask <mention id="m1">Anna</mention>, see <a id="l1" href="x">this</a>.`,
			Links: []newsdoc.Block{mention, seeAlso},
		},
		{
			Name: "plain text",
			Text: `Nothing <strong>inline</strong> here.`,
		},
		{
			Name: "empty text",
		},
		{
			Name:    "mention without a link",
			Text:    `Ask <mention id="m2">Bo</mention>.`,
			Links:   []newsdoc.Block{mention},
			WantErr: `<mention id="m2">`,
		},
		{
			Name:    "anchor without a link",
			Text:    `See <a id="l2">that</a>.`,
			Links:   []newsdoc.Block{seeAlso},
			WantErr: `<a id="l2">`,
		},
		{
			Name:    "mention bound to a see-also link",
			Text:    `Ask <mention id="l1">Anna</mention>.`,
			Links:   []newsdoc.Block{seeAlso},
			WantErr: `bound to a "see-also" link`,
		},
		{
			Name:    "anchor bound to a mention link",
			Text:    `See <a id="m1">that</a>.`,
			Links:   []newsdoc.Block{mention},
			WantErr: `bound to a "mention" link`,
		},
		{
			// The schema refuses an element without an id.
			Name: "element without id",
			Text: `Ask <mention>Anna</mention>.`,
		},
		{
			Name:    "upper case element and attribute",
			Text:    `Ask <MENTION ID="m3">Anna</MENTION>.`,
			Links:   []newsdoc.Block{mention},
			WantErr: `<mention id="m3">`,
		},
		{
			Name:  "self closing element",
			Text:  `Ask <mention id="m1"/>.`,
			Links: []newsdoc.Block{mention},
		},
	}

	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			doc := newsdoc.Document{
				Type: "core/comment",
				Content: []newsdoc.Block{
					{
						Type:  "core/text",
						Data:  map[string]string{"text": c.Text},
						Links: c.Links,
					},
				},
			}

			err := checkInlineLinkIDs(doc)

			switch {
			case c.WantErr == "" && err != nil:
				t.Fatalf("expected no error, got %v", err)
			case c.WantErr != "" && err == nil:
				t.Fatalf("expected an error containing %q", c.WantErr)
			case c.WantErr != "" && !strings.Contains(err.Error(), c.WantErr):
				t.Fatalf("expected an error containing %q, got %v",
					c.WantErr, err)
			}
		})
	}

	// A link in another block does not bind an element.
	doc := newsdoc.Document{
		Type: "core/comment",
		Content: []newsdoc.Block{
			{
				Type: "core/text",
				Data: map[string]string{"text": `Ask <mention id="m1">Anna</mention>.`},
			},
			{
				Type:  "core/text",
				Data:  map[string]string{"text": ""},
				Links: []newsdoc.Block{mention},
			},
		},
	}

	err := checkInlineLinkIDs(doc)
	if err == nil || !strings.Contains(err.Error(), "content block 0") {
		t.Fatalf("expected the first block to be refused, got %v", err)
	}
}
