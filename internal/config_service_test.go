package internal_test

import (
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ttab/elephant-api/newsdoc"
	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/test"
	"github.com/twitchtv/twirp"
)

const extraSettingsSpec = `{
  "version": 1,
  "name": "test-settings-extra",
  "documents": [
    {
      "name": "Hot reload setting",
      "declares": "test/hot-reload-setting",
      "attributes": {
        "title": {}
      }
    }
  ]
}`

const deprecatedSettingsSpec = `{
  "version": 1,
  "name": "test-settings-deprecated",
  "documents": [
    {
      "name": "Deprecated setting",
      "declares": "test/deprecated-setting",
      "attributes": {
        "title": {
          "deprecated": {
            "label": "test-title",
            "doc": "Stop using titles"
          }
        }
      }
    }
  ]
}`

func TestConfiguration(t *testing.T) {
	regenerate := os.Getenv("REGENERATE") == "true"

	testData := filepath.Join("..", "testdata")

	ignoreTimestamps := test.IgnoreTimestamps{
		Fields: []string{"created", "activated"},
	}

	eu := startElephantUser(t)

	ctx := t.Context()

	adminToken := eu.AccessToken(t, elephantine.JWTClaims{
		Scope: "schema_admin",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:  "test",
			Subject: "schema-admin",
		},
	})

	readToken := eu.AccessToken(t, elephantine.JWTClaims{
		Scope: "schema_read",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:  "test",
			Subject: "schema-reader",
		},
	})

	userToken := eu.AccessToken(t, elephantine.JWTClaims{
		Scope: "user",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:  "test",
			Subject: "tester",
		},
	})

	adminCtx, _ := twirp.WithHTTPRequestHeaders(ctx, http.Header{
		"Authorization": []string{"Bearer " + adminToken},
	})
	readCtx, _ := twirp.WithHTTPRequestHeaders(ctx, http.Header{
		"Authorization": []string{"Bearer " + readToken},
	})
	userCtx, _ := twirp.WithHTTPRequestHeaders(ctx, http.Header{
		"Authorization": []string{"Bearer " + userToken},
	})

	// The seeded generation with the embedded schemas should be active.
	active1, err := eu.Configuration.GetActiveConfigGeneration(readCtx,
		&user.GetActiveConfigGenerationRequest{})
	test.Must(t, err, "get active config generation")

	test.TestMessageAgainstGolden(t, regenerate, active1,
		filepath.Join(testData, "config_get_active_1.json"),
		ignoreTimestamps)

	seededID := active1.Generation.Id

	// Register a second generation that references the stored embedded
	// schemas without specs and adds an extra settings schema.
	register1, err := eu.Configuration.RegisterConfigGeneration(adminCtx,
		&user.RegisterConfigGenerationRequest{
			Description: "extra settings schema",
			Schemas: []*user.ConfigGenerationSchema{
				{
					Name:    "se.ecms.user.settings",
					Version: "v1.0.0",
					Usage:   user.SchemaUsage_SCHEMA_USAGE_SETTINGS,
				},
				{
					Name:    "se.ecms.user.messages",
					Version: "v1.0.0",
					Usage:   user.SchemaUsage_SCHEMA_USAGE_MESSAGES,
				},
				{
					Name:    "test-settings-extra",
					Version: "v1.0.0",
					Spec:    extraSettingsSpec,
					Usage:   user.SchemaUsage_SCHEMA_USAGE_SETTINGS,
				},
			},
		})
	test.Must(t, err, "register second generation")

	test.TestMessageAgainstGolden(t, regenerate, register1,
		filepath.Join(testData, "config_register_1.json"),
		ignoreTimestamps)

	extraGenID := register1.Generation.Id

	if register1.Generation.Active {
		t.Fatal("second generation should not be active on registration")
	}

	// Re-registering the same schema set must return the same
	// generation.
	register2, err := eu.Configuration.RegisterConfigGeneration(adminCtx,
		&user.RegisterConfigGenerationRequest{
			Description: "this description is ignored",
			Schemas:     register1.Generation.Schemas,
		})
	test.Must(t, err, "re-register second generation")

	if register2.Generation.Id != extraGenID {
		t.Fatalf("re-registration returned generation %d, expected %d",
			register2.Generation.Id, extraGenID)
	}

	// Registration validation and access control.

	_, err = eu.Configuration.RegisterConfigGeneration(userCtx,
		&user.RegisterConfigGenerationRequest{
			Schemas: register1.Generation.Schemas,
		})
	test.IsTwirpError(t, err, twirp.PermissionDenied)

	_, err = eu.Configuration.RegisterConfigGeneration(readCtx,
		&user.RegisterConfigGenerationRequest{
			Schemas: register1.Generation.Schemas,
		})
	test.IsTwirpError(t, err, twirp.PermissionDenied)

	_, err = eu.Configuration.GetActiveConfigGeneration(userCtx,
		&user.GetActiveConfigGenerationRequest{})
	test.IsTwirpError(t, err, twirp.PermissionDenied)

	_, err = eu.Configuration.RegisterConfigGeneration(adminCtx,
		&user.RegisterConfigGenerationRequest{})
	test.IsTwirpError(t, err, twirp.InvalidArgument)

	_, err = eu.Configuration.RegisterConfigGeneration(adminCtx,
		&user.RegisterConfigGenerationRequest{
			Schemas: []*user.ConfigGenerationSchema{
				{
					Name:    "test-editorial",
					Version: "v1.0.0",
					Spec:    extraSettingsSpec,
					Usage:   user.SchemaUsage_SCHEMA_USAGE_EDITORIAL,
				},
			},
		})
	test.IsTwirpError(t, err, twirp.InvalidArgument)

	_, err = eu.Configuration.RegisterConfigGeneration(adminCtx,
		&user.RegisterConfigGenerationRequest{
			Schemas: []*user.ConfigGenerationSchema{
				{
					Name:    "test-bad-spec",
					Version: "v1.0.0",
					Spec:    `{"version": 1, "nonsense": true}`,
					Usage:   user.SchemaUsage_SCHEMA_USAGE_SETTINGS,
				},
			},
		})
	test.IsTwirpError(t, err, twirp.InvalidArgument)

	_, err = eu.Configuration.RegisterConfigGeneration(adminCtx,
		&user.RegisterConfigGenerationRequest{
			Schemas: []*user.ConfigGenerationSchema{
				{
					Name:    "se.ecms.user.settings",
					Version: "v1.0.0",
					Spec:    extraSettingsSpec,
					Usage:   user.SchemaUsage_SCHEMA_USAGE_SETTINGS,
				},
			},
		})
	test.IsTwirpError(t, err, twirp.InvalidArgument)

	_, err = eu.Configuration.RegisterConfigGeneration(adminCtx,
		&user.RegisterConfigGenerationRequest{
			Schemas: []*user.ConfigGenerationSchema{
				{
					Name:    "not-stored",
					Version: "v1.0.0",
					Usage:   user.SchemaUsage_SCHEMA_USAGE_SETTINGS,
				},
			},
		})
	test.IsTwirpError(t, err, twirp.InvalidArgument)

	_, err = eu.Configuration.RegisterConfigGeneration(adminCtx,
		&user.RegisterConfigGenerationRequest{
			Schemas: []*user.ConfigGenerationSchema{
				{
					Name:    "se.ecms.user.settings",
					Version: "v1.0.0",
					Usage:   user.SchemaUsage_SCHEMA_USAGE_SETTINGS,
				},
				{
					Name:    "se.ecms.user.settings",
					Version: "v1.0.0",
					Usage:   user.SchemaUsage_SCHEMA_USAGE_SETTINGS,
				},
			},
		})
	test.IsTwirpError(t, err, twirp.InvalidArgument)

	// Documents of a type that only the inactive extra generation
	// declares must not validate yet.
	_, err = eu.Settings.UpdateDocument(userCtx, hotReloadDocument())
	test.MustNot(t, err, "update document with inactive schema type")

	// Activate the extra generation and refresh the validator.
	activate1, err := eu.Configuration.ActivateConfigGeneration(adminCtx,
		&user.ActivateConfigGenerationRequest{
			Id: extraGenID,
		})
	test.Must(t, err, "activate second generation")

	test.TestMessageAgainstGolden(t, regenerate, activate1,
		filepath.Join(testData, "config_activate_1.json"),
		ignoreTimestamps)

	_, err = eu.Configuration.ActivateConfigGeneration(adminCtx,
		&user.ActivateConfigGenerationRequest{
			Id: 10000,
		})
	test.IsTwirpError(t, err, twirp.NotFound)

	err = eu.Validator.RefreshSchemas(ctx)
	test.Must(t, err, "refresh schemas")

	_, err = eu.Settings.UpdateDocument(userCtx, hotReloadDocument())
	test.Must(t, err, "update document after schema activation")

	// A long-poll for changes to the already known active generation
	// returns unchanged.
	unchanged, err := eu.Configuration.GetActiveConfigGeneration(readCtx,
		&user.GetActiveConfigGenerationRequest{
			KnownId:     extraGenID,
			WaitSeconds: 1,
			OnlyChanged: true,
		})
	test.Must(t, err, "long-poll unchanged active generation")

	if !unchanged.Unchanged {
		t.Fatal("expected unchanged response for known active generation")
	}

	// Listing and paging.

	list1, err := eu.Configuration.ListConfigGenerations(readCtx,
		&user.ListConfigGenerationsRequest{})
	test.Must(t, err, "list config generations")

	test.TestMessageAgainstGolden(t, regenerate, list1,
		filepath.Join(testData, "config_list_1.json"),
		ignoreTimestamps)

	page1, err := eu.Configuration.ListConfigGenerations(readCtx,
		&user.ListConfigGenerationsRequest{
			PageSize: 1,
		})
	test.Must(t, err, "list config generations with page size")

	if len(page1.Generations) != 1 ||
		page1.Generations[0].Id != extraGenID {
		t.Fatalf("expected a single generation %d in the first page",
			extraGenID)
	}

	page2, err := eu.Configuration.ListConfigGenerations(readCtx,
		&user.ListConfigGenerationsRequest{
			Before: extraGenID,
		})
	test.Must(t, err, "list config generations before the latest")

	if len(page2.Generations) != 1 ||
		page2.Generations[0].Id != seededID {
		t.Fatalf("expected only the seeded generation %d before %d",
			seededID, extraGenID)
	}

	// Schema reads.

	schemaRes, err := eu.Configuration.GetSchema(readCtx,
		&user.GetSchemaRequest{
			Name: "se.ecms.user.settings",
		})
	test.Must(t, err, "get active settings schema")

	if schemaRes.Version != "v1.0.0" ||
		schemaRes.Usage != user.SchemaUsage_SCHEMA_USAGE_SETTINGS {
		t.Fatalf("unexpected version %q or usage %q for settings schema",
			schemaRes.Version, schemaRes.Usage)
	}

	_, err = eu.Configuration.GetSchema(readCtx,
		&user.GetSchemaRequest{
			Name: "no-such-schema",
		})
	test.IsTwirpError(t, err, twirp.NotFound)

	// Long-poll for generation changes while registering and
	// activating a third generation.

	var (
		wg     sync.WaitGroup
		polled *user.GetActiveConfigGenerationResponse
	)

	wg.Go(func() {
		res, err := eu.Configuration.GetActiveConfigGeneration(readCtx,
			&user.GetActiveConfigGenerationRequest{
				KnownId:     extraGenID,
				WaitSeconds: 10,
				OnlyChanged: true,
			})
		test.Must(t, err, "long-poll active generation")

		polled = res
	})

	// Let the polling listener be registered before triggering.
	time.Sleep(50 * time.Millisecond)

	register3, err := eu.Configuration.RegisterConfigGeneration(adminCtx,
		&user.RegisterConfigGenerationRequest{
			Description: "deprecated title schema",
			Activate:    true,
			Schemas: []*user.ConfigGenerationSchema{
				{
					Name:    "se.ecms.user.settings",
					Version: "v1.0.0",
					Usage:   user.SchemaUsage_SCHEMA_USAGE_SETTINGS,
				},
				{
					Name:    "se.ecms.user.messages",
					Version: "v1.0.0",
					Usage:   user.SchemaUsage_SCHEMA_USAGE_MESSAGES,
				},
				{
					Name:    "test-settings-deprecated",
					Version: "v1.0.0",
					Spec:    deprecatedSettingsSpec,
					Usage:   user.SchemaUsage_SCHEMA_USAGE_SETTINGS,
				},
			},
		})
	test.Must(t, err, "register and activate third generation")

	deprecatedGenID := register3.Generation.Id

	if !register3.Generation.Active {
		t.Fatal("third generation should be active on registration")
	}

	wg.Wait()

	if polled.Unchanged || polled.Generation == nil ||
		polled.Generation.Id != deprecatedGenID {
		t.Fatalf("long-poll should have returned generation %d",
			deprecatedGenID)
	}

	err = eu.Validator.RefreshSchemas(ctx)
	test.Must(t, err, "refresh schemas after third generation")

	// The extra settings type was not carried over to the third
	// generation.
	_, err = eu.Settings.UpdateDocument(userCtx, hotReloadDocument())
	test.MustNot(t, err, "update document with type from replaced generation")

	// Usage separation: a settings document type must not validate as
	// an inbox message.
	_, err = eu.Messages.PushInboxMessage(userCtx, &user.PushInboxMessageRequest{
		Recipient: "core://user/tester",
		Payload: &newsdoc.Document{
			Uuid:  "3b482036-39fb-584d-8477-444444444444",
			Type:  "core/view-setting",
			Title: "Not an inbox message",
		},
	})
	test.MustNot(t, err, "push settings document as inbox message")

	// Deprecations: unenforced deprecations are counted but allowed.

	_, err = eu.Settings.UpdateDocument(userCtx, deprecatedDocument())
	test.Must(t, err, "update document with unenforced deprecation")

	count, err := testutil.GatherAndCount(eu.Registry,
		"elephant_user_deprecations_total")
	test.Must(t, err, "gather deprecation metrics")

	if count < 1 {
		t.Fatal("expected the deprecation counter to have been counted")
	}

	_, err = eu.Configuration.UpdateDeprecation(readCtx,
		&user.UpdateDeprecationRequest{
			Deprecation: &user.Deprecation{
				Label:    "test-title",
				Enforced: true,
			},
		})
	test.IsTwirpError(t, err, twirp.PermissionDenied)

	_, err = eu.Configuration.UpdateDeprecation(adminCtx,
		&user.UpdateDeprecationRequest{
			Deprecation: &user.Deprecation{
				Label:    "test-title",
				Enforced: true,
			},
		})
	test.Must(t, err, "enforce deprecation")

	err = eu.Validator.RefreshSchemas(ctx)
	test.Must(t, err, "refresh schemas after enforcing deprecation")

	_, err = eu.Settings.UpdateDocument(userCtx, deprecatedDocument())
	test.MustNot(t, err, "update document with enforced deprecation")

	deprecations, err := eu.Configuration.GetDeprecations(readCtx,
		&user.GetDeprecationsRequest{})
	test.Must(t, err, "get deprecations")

	test.TestMessageAgainstGolden(t, regenerate, deprecations,
		filepath.Join(testData, "config_deprecations_1.json"),
		ignoreTimestamps)

	// Re-registering an existing inactive generation with activate set
	// activates it.
	register4, err := eu.Configuration.RegisterConfigGeneration(adminCtx,
		&user.RegisterConfigGenerationRequest{
			Activate: true,
			Schemas:  register1.Generation.Schemas,
		})
	test.Must(t, err, "re-register second generation with activate")

	if register4.Generation.Id != extraGenID || !register4.Generation.Active {
		t.Fatalf(
			"expected generation %d to be activated on re-registration, got %d (active: %v)",
			extraGenID, register4.Generation.Id,
			register4.Generation.Active,
		)
	}
}

func hotReloadDocument() *user.UpdateDocumentRequest {
	return &user.UpdateDocumentRequest{
		Application:   "se.ecms.local.test.hotreload",
		Type:          "test/hot-reload-setting",
		Key:           "current",
		SchemaVersion: "v1.0.0",
		Payload: &newsdoc.Document{
			Type:  "test/hot-reload-setting",
			Title: "Hot reloaded",
		},
	}
}

func deprecatedDocument() *user.UpdateDocumentRequest {
	return &user.UpdateDocumentRequest{
		Application:   "se.ecms.local.test.deprecated",
		Type:          "test/deprecated-setting",
		Key:           "current",
		SchemaVersion: "v1.0.0",
		Payload: &newsdoc.Document{
			Type:  "test/deprecated-setting",
			Title: "Deprecated title",
		},
	}
}
