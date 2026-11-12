package internal_test

import (
	"testing"

	"github.com/ttab/elephant-user/postgres"
	"github.com/ttab/newsdoc"
)

// TestCommentsSchema validates comment bodies against the embedded
// se.ecms.user.comments schema under the comments usage.
func TestCommentsSchema(t *testing.T) {
	eu := startElephantUser(t)

	ctx := t.Context()

	cases := []struct {
		Name  string
		Doc   func(d *newsdoc.Document)
		Valid bool
	}{
		{
			Name:  "mention and see-also",
			Doc:   func(_ *newsdoc.Document) {},
			Valid: true,
		},
		{
			Name: "application subject",
			Doc: func(d *newsdoc.Document) {
				d.Content[0].Links[0].URI = "core://application/factchecker"
			},
			Valid: true,
		},
		{
			Name: "fully qualified subject",
			Doc: func(d *newsdoc.Document) {
				d.Content[0].Links[0].URI = "keycloak://user/5d3e/a"
			},
			Valid: true,
		},
		{
			Name: "comment reference in see-also data",
			Doc: func(d *newsdoc.Document) {
				d.Content[0].Links[1].Data = map[string]string{
					"thread":  "0b4d8b3b-1f0f-4c3e-9a55-2d1f1b0e0a01",
					"comment": "0b4d8b3b-1f0f-4c3e-9a55-2d1f1b0e0a02",
				}
			},
			Valid: true,
		},
		{
			// textbit leaves trailing empty paragraphs, as the
			// messages schema allows.
			Name: "empty text block",
			Doc: func(d *newsdoc.Document) {
				d.Content[0].Data["text"] = ""
				d.Content[0].Links = nil
			},
			Valid: true,
		},
		{
			// Revisor has no constraint binding an inline id to a
			// link id, so this passes the schema and has to be
			// checked by the service.
			Name: "inline mention without a link",
			Doc: func(d *newsdoc.Document) {
				d.Content[0].Links = d.Content[0].Links[1:]
			},
			Valid: true,
		},
		{
			Name: "mention uri that is not a subject",
			Doc: func(d *newsdoc.Document) {
				d.Content[0].Links[0].URI = "core://author/5d3e"
			},
		},
		{
			Name: "mention without uri",
			Doc: func(d *newsdoc.Document) {
				d.Content[0].Links[0].URI = ""
			},
		},
		{
			Name: "mention with a title",
			Doc: func(d *newsdoc.Document) {
				d.Content[0].Links[0].Title = "Anna"
			},
		},
		{
			Name: "mention with a uuid",
			Doc: func(d *newsdoc.Document) {
				d.Content[0].Links[0].UUID = "9c1b8d0e-6f4a-4b8e-8a8f-0d2c3b4a5e6f"
			},
		},
		{
			Name: "unknown see-also data",
			Doc: func(d *newsdoc.Document) {
				d.Content[0].Links[1].Data = map[string]string{
					"span": "12-40",
				}
			},
		},
		{
			Name: "comment anchor in the text",
			Doc: func(d *newsdoc.Document) {
				d.Content[0].Data["text"] = `Check <comment id="t1">this</comment>.`
			},
		},
		{
			Name: "mention element without id",
			Doc: func(d *newsdoc.Document) {
				d.Content[0].Data["text"] = `Ask <mention>Anna</mention>.`
			},
		},
		{
			Name: "anchor without id",
			Doc: func(d *newsdoc.Document) {
				d.Content[0].Data["text"] = `See <a href="https://example.com">this</a>.`
			},
		},
		{
			Name: "other document type",
			Doc: func(d *newsdoc.Document) {
				d.Type = "core/inbox-message"
			},
		},
	}

	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			doc := validComment()

			c.Doc(&doc)

			res, err := eu.Validator.ValidateDocument(
				ctx, postgres.SchemaUsageComments, &doc)
			if err != nil {
				t.Fatalf("validate document: %v", err)
			}

			switch {
			case c.Valid && len(res) > 0:
				t.Fatalf("expected a valid document, got %v", res)
			case !c.Valid && len(res) == 0:
				t.Fatal("expected validation errors")
			}
		})
	}
}

func validComment() newsdoc.Document {
	return newsdoc.Document{
		UUID: "6f1d1d8e-2b1a-4c55-9d7e-3a2f4b5c6d7e",
		Type: "core/comment",
		Content: []newsdoc.Block{
			{
				Type: "core/text",
				Data: map[string]string{
					"text": `Check this with <mention id="m1">Anna</mention>, ` +
						`see <a id="l1">the <strong>wire</strong></a>.`,
				},
				Links: []newsdoc.Block{
					{
						ID:   "m1",
						Rel:  "mention",
						Type: "tt/keycloak",
						URI:  "core://user/5d3e",
					},
					{
						ID:   "l1",
						Rel:  "see-also",
						Type: "tt/wire",
						UUID: "9c1b8d0e-6f4a-4b8e-8a8f-0d2c3b4a5e6f",
					},
				},
			},
		},
	}
}
