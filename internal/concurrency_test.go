package internal_test

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ttab/elephant-api/newsdoc"
	"github.com/ttab/elephant-user/internal"
	"github.com/ttab/elephantine/test"
)

// TestConcurrentEventLog verifies that a tailer reading the eventlog with
// "id > after_id" sees every entry when several writers commit concurrently.
// With identity-assigned ids a slow transaction could commit a lower id
// after a faster one had already advanced the tailer's cursor, permanently
// hiding the entry; the sequence counter hands out ids in commit order so
// that cannot happen. The load-bearing assertion is the event COUNT - a
// skipped entry shows up as a missing event, while the delivered ids look
// consecutive either way. The race is probabilistic, so this test is not
// guaranteed to fail on a broken implementation, but it exercises the
// invariant under real contention.
func TestConcurrentEventLog(t *testing.T) {
	eu := startElephantUser(t)

	const (
		writers          = 8
		updatesPerWriter = 25
		wantEvents       = writers * updatesPerWriter
	)

	owner := "core://user/tailer"
	ctx := t.Context()

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
				ctx, []string{owner}, afterID, 500)
			if err != nil {
				tailErr = err

				return
			}

			for _, e := range events {
				seen = append(seen, e.ID)
				afterID = e.ID
			}

			if len(events) == 0 {
				time.Sleep(time.Millisecond)
			}
		}
	})

	var (
		start   = make(chan struct{})
		writeWG sync.WaitGroup
	)

	for w := range writers {
		writeWG.Go(func() {
			<-start

			for i := range updatesPerWriter {
				err := eu.Store.UpdateDocument(ctx, internal.DocumentUpdate{
					Owner:         owner,
					Application:   "se.ecms.local.test.concurrency",
					Type:          "core/view-setting",
					Key:           fmt.Sprintf("w%d-%d", w, i),
					SchemaVersion: "v1.0.0",
					Title:         "Concurrent",
					UpdatedBy:     owner,
					Payload:       []byte(`{"type":"core/view-setting"}`),
				})
				if err != nil {
					t.Errorf("writer %d update %d: %v", w, i, err)

					return
				}
			}
		})
	}

	close(start)
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

// TestConcurrentFirstMessage verifies that concurrent pushes to a recipient
// that has a user row but no message_write_lock row yet all succeed with
// gapless ids. This is the reachable variant of the first-message race: for
// a recipient with no user row at all the UpsertUser speculative insert
// serializes the writers before they reach the lock race, so the user row
// must be seeded first. Before the lock row was created with an atomic
// upsert, the race losers hit a primary key violation that aborted the
// transaction and surfaced as an error.
func TestConcurrentFirstMessage(t *testing.T) {
	eu := startElephantUser(t)

	// The race window only exists until the first push for a recipient
	// commits, so run many rounds against fresh recipients rather than
	// one large burst.
	const (
		rounds = 40
		pushes = 12
	)

	ctx := t.Context()

	for round := range rounds {
		recipient := fmt.Sprintf("core://user/fresh-%d", round)

		// Seed the user row without touching message tables.
		err := eu.Store.SetProperties(ctx, recipient, []internal.PropertyUpdate{{
			Application: "se.ecms.local.test.concurrency",
			Key:         "seed",
			Value:       "true",
		}})
		test.Mustf(t, err, "seed user row")

		var (
			start = make(chan struct{})
			wg    sync.WaitGroup
		)

		for i := range pushes {
			wg.Go(func() {
				payload, err := json.Marshal(map[string]string{
					"title": fmt.Sprintf("Message %d", i),
				})
				if err != nil {
					t.Errorf("round %d push %d: marshal payload: %v", round, i, err)

					return
				}

				<-start

				err = eu.Store.InsertMessage(ctx, internal.Message{
					Recipient: recipient,
					Type:      "test",
					Created:   time.Now(),
					CreatedBy: "core://application/test",
					Payload:   payload,
				})
				if err != nil {
					t.Errorf("round %d push %d: %v", round, i, err)
				}
			})
		}

		close(start)
		wg.Wait()

		if t.Failed() {
			return
		}

		msgs, err := eu.Store.ListMessagesAfterID(ctx, recipient, 0, pushes+1)
		test.Mustf(t, err, "list messages")

		if len(msgs) != pushes {
			t.Fatalf("round %d: got %d messages, want %d",
				round, len(msgs), pushes)
		}

		for i, m := range msgs {
			if m.ID != int64(i+1) {
				t.Fatalf("round %d: message %d has id %d, want %d",
					round, i, m.ID, i+1)
			}
		}
	}
}

// TestConcurrentPropertyWrites verifies that overlapping property writes do
// not deadlock. Two orderings are exercised: the eventlog counter must be
// the last lock a transaction takes (a batch writer holding the counter
// while locking further property rows deadlocks against a single-key writer
// holding one of those rows), and property rows must be locked in a stable
// order (opposite-order batches deadlock on the rows alone).
func TestConcurrentPropertyWrites(t *testing.T) {
	eu := startElephantUser(t)

	const rounds = 40

	owner := "core://user/deadlock"
	app := "se.ecms.local.test.concurrency"
	ctx := t.Context()

	prop := func(key, value string) internal.PropertyUpdate {
		return internal.PropertyUpdate{
			Application: app,
			Key:         key,
			Value:       value,
		}
	}

	var (
		start = make(chan struct{})
		wg    sync.WaitGroup
	)

	wg.Go(func() {
		<-start

		for i := range rounds {
			v := fmt.Sprintf("batch-%d", i)

			err := eu.Store.SetProperties(ctx, owner, []internal.PropertyUpdate{
				prop("a", v), prop("b", v), prop("c", v), prop("d", v),
			})
			if err != nil {
				t.Errorf("batch write %d: %v", i, err)

				return
			}
		}
	})

	wg.Go(func() {
		<-start

		for i := range rounds {
			v := fmt.Sprintf("single-%d", i)

			err := eu.Store.SetProperties(ctx, owner, []internal.PropertyUpdate{
				prop("d", v),
			})
			if err != nil {
				t.Errorf("single write %d: %v", i, err)

				return
			}
		}
	})

	wg.Go(func() {
		<-start

		for i := range rounds {
			v := fmt.Sprintf("reverse-%d", i)

			err := eu.Store.SetProperties(ctx, owner, []internal.PropertyUpdate{
				prop("d", v), prop("c", v), prop("b", v), prop("a", v),
			})
			if err != nil {
				t.Errorf("reverse write %d: %v", i, err)

				return
			}
		}
	})

	close(start)
	wg.Wait()
}

// TestConcurrentInboxPushes verifies the two properties the shared-row
// inbox needs under contention. Pushes to several recipients get ids from
// one counter, so a reader whose owners span all of them lists every
// message in strictly increasing id order with none missing; and
// concurrent pushes of the same document to the same recipient store it
// once and all answer with that one id.
func TestConcurrentInboxPushes(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	recipients := []string{
		"core://unit/one", "core://unit/two",
		"core://org/acme", "core://user/someone",
	}

	const pushes = 32

	payloadFor := func(id string, title string) ([]byte, uuid.UUID) {
		t.Helper()

		docUUID := uuid.MustParse("3b482036-39fb-584d-9000-" + id)

		payload, err := json.Marshal(&newsdoc.Document{
			Uuid:  docUUID.String(),
			Type:  "core/inbox-message",
			Title: title,
		})
		test.Mustf(t, err, "marshal payload")

		return payload, docUUID
	}

	// A reader spanning every recipient tails with "id > after_id" WHILE
	// the pushes run. With commit-ordered ids it cannot skip a message; with
	// a sequence or identity column a slow push could commit a lower id
	// after the cursor had moved past it. Listing only after the pushes
	// would prove nothing, since ORDER BY id hides the race.
	reader := internal.InboxReader{Owners: recipients, Subject: "core://user/reader"}

	var (
		seen    []int64
		afterID int64
		tailErr error
		tailWG  sync.WaitGroup
	)

	tailWG.Go(func() {
		deadline := time.Now().Add(30 * time.Second)

		for len(seen) < pushes && time.Now().Before(deadline) {
			msgs, err := eu.Store.ListInboxMessagesAfterID(ctx, reader, afterID, 500)
			if err != nil {
				tailErr = err

				return
			}

			for _, m := range msgs {
				seen = append(seen, m.ID)
				afterID = m.ID
			}

			if len(msgs) == 0 {
				time.Sleep(time.Millisecond)
			}
		}
	})

	var (
		start = make(chan struct{})
		wg    sync.WaitGroup
		mu    sync.Mutex
		got   = make(map[int64]bool, pushes)
	)

	for i := range pushes {
		payload, docUUID := payloadFor(fmt.Sprintf("%012d", i), fmt.Sprintf("Message %d", i))
		recipient := recipients[i%len(recipients)]

		wg.Go(func() {
			<-start

			id, err := eu.Store.InsertInboxMessage(ctx, internal.InboxMessage{
				UUID:      docUUID,
				Recipient: recipient,
				Created:   time.Now(),
				CreatedBy: "core://application/test",
				Payload:   payload,
			})
			if err != nil {
				t.Errorf("push %d: %v", i, err)

				return
			}

			mu.Lock()
			got[id] = true
			mu.Unlock()
		})
	}

	close(start)
	wg.Wait()
	tailWG.Wait()

	if t.Failed() {
		return
	}

	test.Mustf(t, tailErr, "tail inbox messages")

	test.Equalf(t, pushes, len(got), "every push got its own id")
	test.Equalf(t, pushes, len(seen), "the tailer saw every message")

	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("inbox ids not increasing at index %d: %d after %d",
				i, seen[i], seen[i-1])
		}
	}

	for _, id := range seen {
		if !got[id] {
			t.Fatalf("tailed id %d was not returned by any push", id)
		}
	}

	// The same document pushed concurrently to one recipient is stored
	// once; every push is answered with that id.
	const same = 8

	payload, docUUID := payloadFor("ffffffffffff", "Same message")

	var (
		start2 = make(chan struct{})
		ids    = make(chan int64, same)
	)

	for i := range same {
		wg.Go(func() {
			<-start2

			id, err := eu.Store.InsertInboxMessage(ctx, internal.InboxMessage{
				UUID:      docUUID,
				Recipient: "core://unit/one",
				Created:   time.Now(),
				CreatedBy: "core://application/test",
				Payload:   payload,
			})
			if err != nil {
				t.Errorf("same push %d: %v", i, err)

				return
			}

			ids <- id
		})
	}

	close(start2)
	wg.Wait()
	close(ids)

	if t.Failed() {
		return
	}

	first := int64(-1)

	for id := range ids {
		if first == -1 {
			first = id
		}

		test.Equalf(t, first, id, "every concurrent push of one document answers the same id")
	}

	one := internal.InboxReader{Owners: []string{"core://unit/one"}, Subject: "core://user/reader"}

	msgs, err := eu.Store.ListInboxMessagesAfterID(ctx, one, 0, pushes+same)
	test.Mustf(t, err, "list the unit's messages")

	var stored int

	for _, m := range msgs {
		if m.UUID == docUUID {
			stored++
		}
	}

	test.Equalf(t, 1, stored, "the document is stored once")
}
