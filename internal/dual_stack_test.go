package internal_test

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"github.com/ttab/elephant-api/newsdoc"
	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/rpc"
	"github.com/ttab/elephantine/test"
)

const (
	twirpPrefix   = "/twirp"
	settingsPath  = "/elephant.user.Settings/"
	configPath    = "/elephant.user.Configuration/"
	jsonMediaType = "application/json"
)

// rpcResponse is what a raw JSON caller sees: the status and the decoded
// body.
type rpcResponse struct {
	Status int            `json:"status"`
	Body   map[string]any `json:"body"`
}

// postJSON makes a JSON call against a raw path, the way a caller that does
// not use a generated client does. Extra headers go on the request as given.
func (teu *TestElephantUser) postJSON(
	t *testing.T, token string, path string, body string,
	headers http.Header,
) rpcResponse {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(),
		http.MethodPost, teu.BaseURL+path, strings.NewReader(body))
	test.Mustf(t, err, "create the request")

	req.Header.Set("Content-Type", jsonMediaType)
	req.Header.Set("Authorization", "Bearer "+token)

	maps.Copy(req.Header, headers)

	res, err := teu.Client.Do(req)
	test.Mustf(t, err, "perform the request")

	defer func() {
		_ = res.Body.Close()
	}()

	data, err := io.ReadAll(res.Body)
	test.Mustf(t, err, "read the response body")

	out := rpcResponse{Status: res.StatusCode}

	err = json.Unmarshal(data, &out.Body)
	test.Mustf(t, err, "unmarshal the response body %q", string(data))

	return out
}

func userClaims(subject string, scope string) elephantine.JWTClaims {
	return elephantine.JWTClaims{
		Scope: scope,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:  "test",
			Subject: subject,
		},
		Org:   "core://org/test",
		Units: []string{"core://unit/test"},
	}
}

// TestDualStackBodies pins the raw JSON bodies a caller gets on both
// stacks for one successful call and one error. The two spell field names
// differently, Twirp with the proto names (schema_version) and Connect with
// protojson's lowerCamelCase (schemaVersion), and their error envelopes
// differ (msg/meta against message/details). These goldens are what a
// raw-fetch caller moving off /twirp/ is shown, and what stops either
// spelling from drifting unnoticed.
func TestDualStackBodies(t *testing.T) {
	regenerate := os.Getenv("REGENERATE") == "true"
	dataDir := filepath.Join("..", "testdata", t.Name())

	eu := startElephantUser(t)

	token := eu.AccessToken(t, userClaims("tester", "user"))
	ctx := bearerContext(t.Context(), token)

	docApp := "se.ecms.local.test.bodies"
	docType := "core/view-setting"

	_, err := eu.Settings.UpdateDocument(ctx, &user.UpdateDocumentRequest{
		Application:   docApp,
		Type:          docType,
		Key:           "current",
		SchemaVersion: "v1.0.0",
		Payload: &newsdoc.Document{
			Type:  docType,
			Title: "Dual stack",
		},
	})
	test.Mustf(t, err, "create the document")

	getBody := `{"application":"` + docApp + `","type":"` + docType +
		`","key":"current"}`
	missingBody := `{"application":"` + docApp + `","type":"` + docType +
		`","key":"missing"}`

	// The timestamps differ between runs; everything else in the document
	// is fixed by the request.
	scrub := func(res rpcResponse) rpcResponse {
		doc, ok := res.Body["document"].(map[string]any)
		if ok {
			delete(doc, "created")
			delete(doc, "updated")
		}

		return res
	}

	twirpRes := scrub(eu.postJSON(t, token,
		twirpPrefix+settingsPath+"GetDocument", getBody, nil))

	test.AgainstGolden(t, regenerate, twirpRes,
		filepath.Join(dataDir, "get-document-twirp.json"))

	connectRes := scrub(eu.postJSON(t, token,
		settingsPath+"GetDocument", getBody, nil))

	test.AgainstGolden(t, regenerate, connectRes,
		filepath.Join(dataDir, "get-document-connect.json"))

	twirpErr := eu.postJSON(t, token,
		twirpPrefix+settingsPath+"GetDocument", missingBody, nil)

	test.AgainstGolden(t, regenerate, twirpErr,
		filepath.Join(dataDir, "not-found-twirp.json"))

	connectErr := eu.postJSON(t, token,
		settingsPath+"GetDocument", missingBody, nil)

	test.AgainstGolden(t, regenerate, connectErr,
		filepath.Join(dataDir, "not-found-connect.json"))

	// An error with metadata: Twirp carries it as "meta", Connect as an
	// ErrorMeta detail.
	noPayloadBody := `{"application":"` + docApp + `","type":"` + docType +
		`","key":"current","schemaVersion":"v1.0.0"}`

	twirpMeta := eu.postJSON(t, token,
		twirpPrefix+settingsPath+"UpdateDocument", noPayloadBody, nil)

	test.AgainstGolden(t, regenerate, twirpMeta,
		filepath.Join(dataDir, "required-argument-twirp.json"))

	connectMeta := eu.postJSON(t, token,
		settingsPath+"UpdateDocument", noPayloadBody, nil)

	test.AgainstGolden(t, regenerate, connectMeta,
		filepath.Join(dataDir, "required-argument-connect.json"))
}

// TestDualStackErrorParity runs the error paths over both stacks against
// the same server and checks that a caller cannot tell them apart. The
// handlers still return Twirp errors, which the Connect mount translates
// on the way out, so this is what keeps that translation honest.
func TestDualStackErrorParity(t *testing.T) {
	eu := startElephantUser(t)

	twirpClients := newClients(stackTwirp, eu.Client, eu.BaseURL)
	connectClients := newClients(stackConnect, eu.Client, eu.BaseURL)

	userToken := eu.AccessToken(t, userClaims("tester", "user"))
	noScopeToken := eu.AccessToken(t, userClaims("nobody", ""))

	userCtx := bearerContext(t.Context(), userToken)
	noScopeCtx := bearerContext(t.Context(), noScopeToken)

	// check asserts the code on both stacks and that the errors match.
	check := func(t *testing.T, code connect.Code, twirpErr, connectErr error) {
		t.Helper()

		test.IsRPCError(t, twirpErr, code)
		test.IsRPCError(t, connectErr, code)
		test.ErrorParity(t, twirpErr, connectErr)
	}

	t.Run("missing scope", func(t *testing.T) {
		_, twirpErr := twirpClients.Settings.GetDocument(noScopeCtx,
			&user.GetDocumentRequest{})
		_, connectErr := connectClients.Settings.GetDocument(noScopeCtx,
			&user.GetDocumentRequest{})

		check(t, connect.CodePermissionDenied, twirpErr, connectErr)
	})

	t.Run("invalid argument", func(t *testing.T) {
		req := &user.DeleteInboxMessageRequest{Id: 0}

		_, twirpErr := twirpClients.Messages.DeleteInboxMessage(userCtx, req)
		_, connectErr := connectClients.Messages.DeleteInboxMessage(userCtx, req)

		check(t, connect.CodeInvalidArgument, twirpErr, connectErr)
	})

	t.Run("not found", func(t *testing.T) {
		req := &user.GetDocumentRequest{
			Application: "se.ecms.local.test.parity",
			Type:        "core/view-setting",
			Key:         "missing",
		}

		_, twirpErr := twirpClients.Settings.GetDocument(userCtx, req)
		_, connectErr := connectClients.Settings.GetDocument(userCtx, req)

		check(t, connect.CodeNotFound, twirpErr, connectErr)
	})

	t.Run("required argument", func(t *testing.T) {
		req := &user.UpdateDocumentRequest{
			Application:   "se.ecms.local.test.parity",
			Type:          "core/view-setting",
			Key:           "current",
			SchemaVersion: "v1.0.0",
		}

		_, twirpErr := twirpClients.Settings.UpdateDocument(userCtx, req)
		_, connectErr := connectClients.Settings.UpdateDocument(userCtx, req)

		check(t, connect.CodeInvalidArgument, twirpErr, connectErr)

		test.Equalf(t, "payload", rpc.Meta(connectErr)["argument"],
			"name the missing argument in the metadata")
	})

	t.Run("validation errors", func(t *testing.T) {
		req := &user.UpdateDocumentRequest{
			Application:   "se.ecms.local.test.parity",
			Type:          "core/view-setting",
			Key:           "current",
			SchemaVersion: "v1.0.0",
			Payload: &newsdoc.Document{
				Type:  "core/view-setting",
				Title: "Invalid",
				Meta: []*newsdoc.Block{
					{Type: "test/not-a-declared-block"},
				},
			},
		}

		_, twirpErr := twirpClients.Settings.UpdateDocument(userCtx, req)
		_, connectErr := connectClients.Settings.UpdateDocument(userCtx, req)

		check(t, connect.CodeInvalidArgument, twirpErr, connectErr)

		// The individual errors are numbered from zero, and err_count
		// says how many of them there are.
		meta := rpc.Meta(connectErr)

		count, err := strconv.Atoi(meta["err_count"])
		test.Mustf(t, err, "read the err_count metadata")

		for i := range count {
			test.Equalf(t, false, meta[strconv.Itoa(i)] == "",
				"describe validation error %d", i)
		}
	})

	t.Run("schema argument", func(t *testing.T) {
		adminCtx := bearerContext(t.Context(),
			eu.AccessToken(t, userClaims("admin", "schema_admin")))

		req := &user.RegisterConfigGenerationRequest{
			Schemas: []*user.ConfigGenerationSchema{
				{
					Name:    "se.ecms.user.settings",
					Version: "v1.0.0",
					Usage:   user.SchemaUsage_SCHEMA_USAGE_SETTINGS,
				},
				{
					Name:    "test.unknown",
					Version: "v1.0.0",
					Usage:   user.SchemaUsage_SCHEMA_USAGE_SETTINGS,
				},
			},
		}

		_, twirpErr := twirpClients.Configuration.RegisterConfigGeneration(adminCtx, req)
		_, connectErr := connectClients.Configuration.RegisterConfigGeneration(adminCtx, req)

		check(t, connect.CodeInvalidArgument, twirpErr, connectErr)

		test.Equalf(t, "schemas.1.spec", rpc.Meta(connectErr)["argument"],
			"name the schema entry that lacks a spec")
	})

	t.Run("wrong owner", func(t *testing.T) {
		req := &user.GetDocumentRequest{
			Owner:       "core://org/other",
			Application: "se.ecms.local.test.parity",
			Type:        "core/view-setting",
			Key:         "current",
		}

		_, twirpErr := twirpClients.Settings.GetDocument(userCtx, req)
		_, connectErr := connectClients.Settings.GetDocument(userCtx, req)

		check(t, connect.CodePermissionDenied, twirpErr, connectErr)
	})
}

// TestConnectDeadline checks that a long poll ended by the deadline the
// caller set is answered deadline_exceeded, not canceled. Twirp has no
// timeout header; Connect turns Connect-Timeout-Ms into the handler's
// context deadline, so the waiting RPCs are where the difference shows. A
// caller that retries a timeout but gives up on a cancellation cannot tell
// the two apart if both come back canceled.
func TestConnectDeadline(t *testing.T) {
	eu := startElephantUser(t)

	timeout := http.Header{"Connect-Timeout-Ms": []string{"300"}}

	userToken := eu.AccessToken(t, userClaims("tester", "user"))

	// Nothing is written to the eventlog, so only the deadline can end
	// the poll.
	res := eu.postJSON(t, userToken, settingsPath+"PollEventLog",
		`{"afterId":-1}`, timeout)

	test.Equalf(t, http.StatusGatewayTimeout, res.Status,
		"answer a deadline on the eventlog poll with 504")
	test.Equalf(t, "deadline_exceeded", res.Body["code"],
		"report the deadline rather than a cancellation")

	// The active generation long poll waits the same way, and used to
	// report a cancelled wait as an internal error.
	readToken := eu.AccessToken(t, userClaims("reader", "schema_read"))
	readCtx := bearerContext(t.Context(), readToken)

	active, err := eu.Configuration.GetActiveConfigGeneration(readCtx,
		&user.GetActiveConfigGenerationRequest{})
	test.Mustf(t, err, "get the active generation")

	activeID, err := json.Marshal(active.Generation.Id)
	test.Mustf(t, err, "encode the generation id")

	res = eu.postJSON(t, readToken, configPath+"GetActiveConfigGeneration",
		`{"knownId":`+string(activeID)+`,"waitSeconds":10}`, timeout)

	test.Equalf(t, http.StatusGatewayTimeout, res.Status,
		"answer a deadline on the generation poll with 504")
	test.Equalf(t, "deadline_exceeded", res.Body["code"],
		"report the deadline rather than a cancellation")
}

// TestConnectGRPC checks that the gRPC protocol Connect serves on the
// Connect paths is reachable. It needs HTTP/2, which a plaintext listener
// only speaks if it declares unencrypted HTTP/2; nothing else in the suite
// would notice if that went away, since Twirp and Connect are HTTP/1.1.
func TestConnectGRPC(t *testing.T) {
	eu := startElephantUser(t)

	var protocols http.Protocols

	protocols.SetUnencryptedHTTP2(true)

	h2 := &http.Client{
		Transport: &http.Transport{
			Protocols: &protocols,
		},
	}

	t.Cleanup(h2.CloseIdleConnections)

	clients := newClients(stackConnect, h2, eu.BaseURL, connect.WithGRPC())

	ctx := bearerContext(t.Context(),
		eu.AccessToken(t, userClaims("tester", "user")))

	_, err := clients.Settings.SetProperties(ctx, &user.SetPropertiesRequest{
		Properties: []*user.PropertyUpdate{
			{Application: "se.ecms.local.test.grpc", Key: "theme", Value: "dark"},
		},
	})
	test.Mustf(t, err, "set a property over gRPC")

	res, err := clients.Settings.GetProperties(ctx, &user.GetPropertiesRequest{
		Application: "se.ecms.local.test.grpc",
	})
	test.Mustf(t, err, "get the properties over gRPC")

	test.Equalf(t, 1, len(res.Properties), "return the property over gRPC")
	test.Equalf(t, "dark", res.Properties[0].Value, "return the value over gRPC")
}
