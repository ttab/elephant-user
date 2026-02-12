package internal_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephant-api/newsdoc"
	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephant-user/internal"
	"github.com/ttab/elephant-user/schema"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/pg"
	"github.com/ttab/elephantine/test"
	"github.com/ttab/eltest"
	"github.com/twitchtv/twirp"
)

func TestService(t *testing.T) {
	regenerate := os.Getenv("REGENERATE") == "true"

	testData := filepath.Join("..", "testdata")

	eu := startElephantUser(t)

	subjectClaim := "tester"
	recipient := "core://user/" + subjectClaim
	orgTest := "core://org/test"

	userToken := eu.AccessToken(t, elephantine.JWTClaims{
		Scope: "user",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:  "test",
			Subject: subjectClaim,
		},
		Org:   orgTest,
		Units: []string{"core://unit/test"},
	})

	ctx := t.Context()

	authCtx, _ := twirp.WithHTTPRequestHeaders(ctx, http.Header{
		"Authorization": []string{"Bearer " + userToken},
	})

	wg := sync.WaitGroup{}
	wg.Add(1)

	// Inbox messages

	go func() {
		defer wg.Done()

		polledInboxMessages1, err := eu.Messages.PollInboxMessages(authCtx, &user.PollInboxMessagesRequest{})
		test.Must(t, err, "poll inbox messages")

		test.TestMessageAgainstGolden(t, regenerate, polledInboxMessages1,
			filepath.Join(testData, "poll_inbox_messages_1.json"),
			test.IgnoreTimestamps{})
	}()

	_, err := eu.Messages.PushInboxMessage(authCtx, &user.PushInboxMessageRequest{
		Recipient: recipient,
		Payload: &newsdoc.Document{
			Uuid:  "3b482036-39fb-584d-8477-111111111111",
			Type:  "tt/inbox-message",
			Uri:   "message://inbox/1",
			Title: "Inbox Message 1",
			Content: []*newsdoc.Block{
				{
					Type: "core/text",
					Data: map[string]string{
						"text": "Inbox Message Body",
					},
				},
			},
		},
	})
	test.Must(t, err, "push inbox message")

	_, err = eu.Messages.PushInboxMessage(authCtx, &user.PushInboxMessageRequest{
		Recipient: recipient,
		Payload: &newsdoc.Document{
			Uuid:  "3b482036-39fb-584d-8477-222222222222",
			Type:  "tt/inbox-message",
			Uri:   "message://inbox/2",
			Title: "Inbox Message 2",
		},
	})
	test.Must(t, err, "push inbox message")

	_, err = eu.Messages.PushInboxMessage(authCtx, &user.PushInboxMessageRequest{
		Recipient: recipient,
		Payload: &newsdoc.Document{
			Uuid:  "3b482036-39fb-584d-8477-333333333333",
			Type:  "tt/inbox-message",
			Uri:   "message://inbox/3",
			Title: "Inbox Message 3",
		},
	})
	test.Must(t, err, "push inbox message")

	_, err = eu.Messages.UpdateInboxMessage(authCtx, &user.UpdateInboxMessageRequest{
		Id:     1,
		IsRead: true,
	})
	test.Must(t, err, "mark inbox message as read")

	listedInboxMessages1, err := eu.Messages.ListInboxMessages(authCtx, &user.ListInboxMessagesRequest{})
	test.Must(t, err, "list inbox messages")
	test.TestMessageAgainstGolden(t, regenerate, listedInboxMessages1,
		filepath.Join(testData, "list_inbox_messages_1.json"),
		test.IgnoreTimestamps{})

	listedInboxMessages2, err := eu.Messages.ListInboxMessages(authCtx, &user.ListInboxMessagesRequest{
		BeforeId: 3,
	})
	test.Must(t, err, "list inbox messages before id")
	test.TestMessageAgainstGolden(t, regenerate, listedInboxMessages2,
		filepath.Join(testData, "list_inbox_messages_2.json"),
		test.IgnoreTimestamps{})

	_, err = eu.Messages.DeleteInboxMessage(authCtx, &user.DeleteInboxMessageRequest{
		Id: 1,
	})
	test.Must(t, err, "delete inbox message")

	listedInboxMessages3, err := eu.Messages.ListInboxMessages(authCtx, &user.ListInboxMessagesRequest{})
	test.Must(t, err, "list inbox messages after deletion")
	test.TestMessageAgainstGolden(t, regenerate, listedInboxMessages3,
		filepath.Join(testData, "list_inbox_messages_3.json"),
		test.IgnoreTimestamps{})

	// Messages

	wg.Add(1)

	go func() {
		defer wg.Done()

		polledMessages1, err := eu.Messages.PollMessages(authCtx, &user.PollMessagesRequest{})
		test.Must(t, err, "poll messages")

		test.TestMessageAgainstGolden(t, regenerate, polledMessages1,
			filepath.Join(testData, "poll_messages_1.json"),
			test.IgnoreTimestamps{})
	}()

	_, err = eu.Messages.PushMessage(authCtx, &user.PushMessageRequest{
		Recipient: recipient,
		Type:      "validation_error",
		DocUuid:   "f6106dd5-db55-596a-bc26-d7f3bdf783e5",
		DocType:   "core/article",
		Payload: map[string]string{
			"message": "fix document",
		},
	})
	test.Must(t, err, "push message")

	// Settings

	docApp := "se.ecms.local.test.assignments"
	docType := "core/view-setting"
	docKey := "current"

	_, err = eu.Settings.UpdateDocument(authCtx, &user.UpdateDocumentRequest{
		Application:   docApp,
		Type:          docType,
		Key:           docKey,
		SchemaVersion: "v1.0.0",
		Payload: &newsdoc.Document{
			Type:  docType,
			Title: "Assignments",
			Meta: []*newsdoc.Block{
				{
					Type:        docType,
					Contenttype: "assignment-type",
					Role:        "filter",
					Value:       "picture",
				},
				{
					Type:  docType,
					Role:  "sortorder",
					Value: "created",
					Data: map[string]string{
						"order": "desc",
					},
				},
			},
		},
	})
	test.Must(t, err, "update document")

	getDocument1, err := eu.Settings.GetDocument(authCtx, &user.GetDocumentRequest{
		Application: docApp,
		Type:        docType,
		Key:         docKey,
	})
	test.Must(t, err, "get document")

	test.TestMessageAgainstGolden(t, regenerate, getDocument1,
		filepath.Join(testData, "get_document_1.json"),
		test.IgnoreTimestamps{})

	listDocuments1, err := eu.Settings.ListDocuments(authCtx, &user.ListDocumentsRequest{
		Application: docApp,
	})
	test.Must(t, err, "list documents")

	test.TestMessageAgainstGolden(t, regenerate, listDocuments1,
		filepath.Join(testData, "list_documents_1.json"),
		test.IgnoreTimestamps{})

	// Properties

	propApp := "se.ecms.local.test.preferences"

	_, err = eu.Settings.SetProperties(authCtx, &user.SetPropertiesRequest{
		Properties: []*user.PropertyUpdate{
			{Application: propApp, Key: "prop1", Value: "val1"},
			{Application: propApp, Key: "prop2", Value: "val2"},
		},
	})
	test.Must(t, err, "set properties")

	getProperties1, err := eu.Settings.GetProperties(authCtx, &user.GetPropertiesRequest{
		Application: propApp,
		Keys:        []string{"prop1", "prop2"},
	})
	test.Must(t, err, "get properties")

	test.TestMessageAgainstGolden(t, regenerate, getProperties1,
		filepath.Join(testData, "get_properties_1.json"),
		test.IgnoreTimestamps{})

	_, err = eu.Settings.DeleteProperties(authCtx, &user.DeletePropertiesRequest{
		Properties: []*user.PropertyDelete{
			{Application: propApp, Key: "prop1"},
		},
	})
	test.Must(t, err, "delete properties")

	getProperties2, err := eu.Settings.GetProperties(authCtx, &user.GetPropertiesRequest{
		Application: propApp,
		Keys:        []string{"prop1", "prop2"},
	})
	test.Must(t, err, "get properties after deletion")

	test.TestMessageAgainstGolden(t, regenerate, getProperties2,
		filepath.Join(testData, "get_properties_2.json"),
		test.IgnoreTimestamps{})

	// Event log

	polledEventLog1, err := eu.Settings.PollEventLog(authCtx, &user.PollEventLogRequest{})
	test.Must(t, err, "poll event log")

	test.TestMessageAgainstGolden(t, regenerate, polledEventLog1,
		filepath.Join(testData, "poll_event_log_1.json"),
		test.IgnoreTimestamps{})

	wg.Add(1)

	go func() {
		defer wg.Done()

		polledEventLog2, err := eu.Settings.PollEventLog(authCtx, &user.PollEventLogRequest{AfterId: -1})
		test.Must(t, err, "poll event log for document deletion")

		test.TestMessageAgainstGolden(t, regenerate, polledEventLog2,
			filepath.Join(testData, "poll_event_log_2.json"),
			test.IgnoreTimestamps{})
	}()

	_, err = eu.Settings.DeleteDocument(authCtx, &user.DeleteDocumentRequest{
		Application: docApp,
		Type:        docType,
		Key:         docKey,
	})
	test.Must(t, err, "delete document")

	// Admin and shared access

	adminToken := eu.AccessToken(t, elephantine.JWTClaims{
		Scope: "user doc_admin",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:  "test",
			Subject: "admin-tester",
		},
		Units: []string{"core://unit/test"},
		Org:   orgTest,
	})

	adminAuthCtx, _ := twirp.WithHTTPRequestHeaders(ctx, http.Header{
		"Authorization": []string{"Bearer " + adminToken},
	})

	otherUserToken := eu.AccessToken(t, elephantine.JWTClaims{
		Scope: "user",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:  "test",
			Subject: "other-user",
		},
		Org: "core://org/other",
	})

	otherUserAuthCtx, _ := twirp.WithHTTPRequestHeaders(ctx, http.Header{
		"Authorization": []string{"Bearer " + otherUserToken},
	})

	_, err = eu.Settings.UpdateDocument(authCtx, &user.UpdateDocumentRequest{
		Owner:         orgTest,
		Application:   docApp,
		Type:          docType,
		Key:           docKey,
		SchemaVersion: "v1.0.0",
		Payload:       &newsdoc.Document{Type: docType, Title: "Org wide access"},
	})
	test.MustNot(t, err, "create shared document as non-admin")

	_, err = eu.Settings.UpdateDocument(adminAuthCtx, &user.UpdateDocumentRequest{
		Owner:         orgTest,
		Application:   docApp,
		Type:          docType,
		Key:           docKey,
		SchemaVersion: "v1.0.0",
		Payload:       &newsdoc.Document{Type: docType, Title: "Org wide access"},
	})
	test.Must(t, err, "create shared document as admin")

	_, err = eu.Settings.GetDocument(authCtx, &user.GetDocumentRequest{
		Owner:       orgTest,
		Application: docApp,
		Type:        docType,
		Key:         docKey,
	})
	test.Must(t, err, "get shared document as non-admin")

	// List documents as non-admin should include shared doc.
	listDocuments2, err := eu.Settings.ListDocuments(authCtx, &user.ListDocumentsRequest{
		Application: docApp,
	})
	test.Must(t, err, "list documents as non-admin")

	test.TestMessageAgainstGolden(t, regenerate, listDocuments2,
		filepath.Join(testData, "list_documents_2.json"),
		test.IgnoreTimestamps{})

	_, err = eu.Settings.GetDocument(otherUserAuthCtx, &user.GetDocumentRequest{
		Owner:       orgTest,
		Application: docApp,
		Type:        docType,
		Key:         docKey,
	})
	test.MustNot(t, err, "get shared doc from org it doesn't belong to")

	_, err = eu.Settings.UpdateDocument(adminAuthCtx, &user.UpdateDocumentRequest{
		Owner:         "core://unit/other",
		Application:   docApp,
		Type:          docType,
		Key:           docKey,
		SchemaVersion: "v1.0.0",
		Payload:       &newsdoc.Document{Type: docType, Title: "Unit wide access"},
	})
	test.MustNot(t, err, "create shared doc for unit it doesn't belong to")

	// Event log for shared documents

	wg.Add(1)

	go func() {
		defer wg.Done()

		polledEventLog3, err := eu.Settings.PollEventLog(authCtx, &user.PollEventLogRequest{AfterId: -1})
		test.Must(t, err, "poll event log for shared document update")

		test.TestMessageAgainstGolden(t, regenerate, polledEventLog3,
			filepath.Join(testData, "poll_event_log_3.json"),
			test.IgnoreTimestamps{})
	}()

	_, err = eu.Settings.UpdateDocument(adminAuthCtx, &user.UpdateDocumentRequest{
		Owner:         orgTest,
		Application:   docApp,
		Type:          docType,
		Key:           docKey,
		SchemaVersion: "v1.0.0",
		Payload: &newsdoc.Document{
			Type: docType, Title: "Event Log Trigger Doc",
		},
	})
	test.Must(t, err, "update shared document as admin")

	wg.Wait()
}

type TestElephantUser struct {
	JWTKey   *ecdsa.PrivateKey
	Messages user.Messages
	Settings user.Settings
}

func (teu *TestElephantUser) AccessToken(t *testing.T, claims elephantine.JWTClaims) string {
	t.Helper()

	token := jwt.NewWithClaims(jwt.SigningMethodES384, claims)

	ss, err := token.SignedString(teu.JWTKey)
	test.Must(t, err, "sign JWT token")

	return ss
}

func startElephantUser(t *testing.T) TestElephantUser {
	t.Helper()

	ctx := t.Context()
	logger := slog.New(test.NewLogHandler(t, slog.LevelInfo))

	pgTestInstance := eltest.NewPostgres(t)
	pgEnv := pgTestInstance.Database(t, schema.Migrations, true)

	dbpool, err := pgxpool.New(ctx, pgEnv.PostgresURI)
	test.Must(t, err, "create connection pool")

	t.Cleanup(func() {
		// We don't want to block cleanup waiting for pool.
		go dbpool.Close()
	})

	store := internal.NewPGStore(logger, dbpool)

	go pg.Subscribe(
		ctx, logger, dbpool,
		store.Messages,
		store.InboxMessages,
		store.EventLog,
	)

	validator, err := internal.NewValidator(ctx)
	test.Must(t, err, "create validator")

	apiServer, client := elephantine.NewTestAPIServer(t, logger)

	jwtKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	test.Must(t, err, "create signing key")

	auth := elephantine.NewStaticAuthInfoParser(ctx, jwtKey.PublicKey,
		elephantine.JWTAuthInfoParserOptions{
			Issuer: "test",
		})

	reg := prometheus.NewRegistry()

	service := internal.NewService(logger, store, validator)

	err = internal.Run(ctx, internal.Parameters{
		Logger:         logger,
		APIServer:      apiServer,
		AuthInfoParser: auth,
		Registerer:     reg,
		Service:        service,
	})
	test.Must(t, err, "run application")

	messages := user.NewMessagesProtobufClient("http://"+apiServer.Addr(), client)
	settings := user.NewSettingsProtobufClient("http://"+apiServer.Addr(), client)

	return TestElephantUser{
		JWTKey:   jwtKey,
		Messages: messages,
		Settings: settings,
	}
}

func TestMain(m *testing.M) {
	exitVal := m.Run()

	err := eltest.PurgeBackingServices()
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"failed to clean up backend services: %v\n", err)
	}

	os.Exit(exitVal)
}
