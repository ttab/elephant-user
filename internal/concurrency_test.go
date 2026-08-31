package internal_test

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/ttab/elephant-api/newsdoc"
	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/test"
	"github.com/twitchtv/twirp"
)

// TestConcurrentEventLog verifies that a tailer reading the eventlog with
// "id > after_id" never skips an entry when several writers commit
// concurrently. With identity-assigned ids a slow transaction could commit a
// lower id after a faster one had already advanced the tailer's cursor; the
// sequence counter hands out ids in commit order so that cannot happen.
func TestConcurrentEventLog(t *testing.T) {
	eu := startElephantUser(t)

	const (
		writers         = 4
		updatesPerWrite = 25
		wantEvents      = writers * updatesPerWrite
	)

	subjectClaim := "tailer"
	owner := "core://user/" + subjectClaim

	userToken := eu.AccessToken(t, elephantine.JWTClaims{
		Scope: "user",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:  "test",
			Subject: subjectClaim,
		},
	})

	ctx := t.Context()

	authCtx, _ := twirp.WithHTTPRequestHeaders(ctx, http.Header{
		"Authorization": []string{"Bearer " + userToken},
	})

	var (
		seen    []int64
		afterID int64
		tailErr error
		tailWG  sync.WaitGroup
	)

	tailWG.Go(func() {
		deadline := time.Now().Add(30 * time.Second)

		for len(seen) < wantEvents && time.Now().Before(deadline) {
			events, err := eu.Store.GetEventLogEntriesAfterID(
				ctx, []string{owner}, afterID, 100)
			if err != nil {
				tailErr = err

				return
			}

			for _, e := range events {
				seen = append(seen, e.ID)
				afterID = e.ID
			}

			if len(events) == 0 {
				time.Sleep(5 * time.Millisecond)
			}
		}
	})

	var writeWG sync.WaitGroup

	for w := range writers {
		writeWG.Go(func() {
			for i := range updatesPerWrite {
				_, err := eu.Settings.UpdateDocument(authCtx, &user.UpdateDocumentRequest{
					Application:   "se.ecms.local.test.concurrency",
					Type:          "core/view-setting",
					Key:           fmt.Sprintf("w%d-%d", w, i),
					SchemaVersion: "v1.0.0",
					Payload: &newsdoc.Document{
						Type:  "core/view-setting",
						Title: "Concurrent",
					},
				})
				if err != nil {
					t.Errorf("writer %d update %d: %v", w, i, err)

					return
				}
			}
		})
	}

	writeWG.Wait()
	tailWG.Wait()

	test.Mustf(t, tailErr, "tail eventlog")

	if len(seen) != wantEvents {
		t.Fatalf("tailer saw %d events, want %d", len(seen), wantEvents)
	}

	for i := 1; i < len(seen); i++ {
		if seen[i] != seen[i-1]+1 {
			t.Fatalf("eventlog ids not consecutive at index %d: %d after %d",
				i, seen[i], seen[i-1])
		}
	}
}

// TestConcurrentFirstMessage verifies that concurrent first-ever pushes to a
// recipient all succeed and get gapless ids. Before the write lock row was
// created up front, the losers of the race hit a primary key violation that
// aborted the transaction.
func TestConcurrentFirstMessage(t *testing.T) {
	eu := startElephantUser(t)

	const pushes = 10

	recipient := "core://user/fresh"

	userToken := eu.AccessToken(t, elephantine.JWTClaims{
		Scope: "user",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:  "test",
			Subject: "pusher",
		},
	})

	ctx := t.Context()

	authCtx, _ := twirp.WithHTTPRequestHeaders(ctx, http.Header{
		"Authorization": []string{"Bearer " + userToken},
	})

	var wg sync.WaitGroup

	for i := range pushes {
		wg.Go(func() {
			_, err := eu.Messages.PushInboxMessage(authCtx, &user.PushInboxMessageRequest{
				Recipient: recipient,
				Payload: &newsdoc.Document{
					Uuid:  fmt.Sprintf("3b482036-39fb-584d-8477-%012d", i),
					Type:  "core/inbox-message",
					Uri:   fmt.Sprintf("message://inbox/%d", i),
					Title: fmt.Sprintf("Inbox Message %d", i),
				},
			})
			if err != nil {
				t.Errorf("push %d: %v", i, err)
			}
		})
	}

	wg.Wait()

	msgs, err := eu.Store.ListInboxMessagesAfterID(ctx, recipient, 0, 100)
	test.Mustf(t, err, "list inbox messages")

	if len(msgs) != pushes {
		t.Fatalf("got %d inbox messages, want %d", len(msgs), pushes)
	}

	for i, m := range msgs {
		if m.ID != int64(i+1) {
			t.Fatalf("inbox message %d has id %d, want %d", i, m.ID, i+1)
		}
	}
}
