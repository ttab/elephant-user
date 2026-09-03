package internal_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

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
				<-start

				err := eu.Store.InsertInboxMessage(ctx, internal.InboxMessage{
					Recipient: recipient,
					Created:   time.Now(),
					CreatedBy: "core://application/test",
					Updated:   time.Now(),
					Payload: &newsdoc.Document{
						Uuid: fmt.Sprintf(
							"3b482036-39fb-584d-%04d-%012d", round, i),
						Type:  "core/inbox-message",
						Title: fmt.Sprintf("Inbox Message %d", i),
					},
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

		msgs, err := eu.Store.ListInboxMessagesAfterID(ctx, recipient, 0, pushes+1)
		test.Mustf(t, err, "list inbox messages")

		if len(msgs) != pushes {
			t.Fatalf("round %d: got %d inbox messages, want %d",
				round, len(msgs), pushes)
		}

		for i, m := range msgs {
			if m.ID != int64(i+1) {
				t.Fatalf("round %d: inbox message %d has id %d, want %d",
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
