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

	eu := startElepantUser(t)

	subjectClaim := "tester"
	recipient := "core://user/" + subjectClaim

	token := eu.AccessToken(t, elephantine.JWTClaims{
		Scope: "user",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:  "test",
			Subject: subjectClaim,
		},
	})

	ctx := t.Context()
	authCtx, _ := twirp.WithHTTPRequestHeaders(ctx, http.Header{
		"Authorization": []string{"Bearer " + token},
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

	wg.Wait()
}

type TestElephantUser struct {
	JWTKey   *ecdsa.PrivateKey
	Messages user.Messages
}

func (teu *TestElephantUser) AccessToken(t *testing.T, claims elephantine.JWTClaims) string {
	t.Helper()

	token := jwt.NewWithClaims(jwt.SigningMethodES384, claims)

	ss, err := token.SignedString(teu.JWTKey)
	test.Must(t, err, "sign JWT token")

	return ss
}

func startElepantUser(t *testing.T) TestElephantUser {
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

	return TestElephantUser{
		JWTKey:   jwtKey,
		Messages: messages,
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
