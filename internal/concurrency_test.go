package internal_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
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

// TestConcurrentCommentThreadCreate verifies that clients racing to create
// the same thread, each with its own first comment, all succeed and land in
// one thread. The thread insert does nothing on conflict and the thread is
// read back under its row lock, so the losers attach to the winner's
// thread; a plain insert fails them with a primary key violation.
func TestConcurrentCommentThreadCreate(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	const (
		rounds  = 20
		writers = 8
	)

	docUUID := uuid.New()

	for round := range rounds {
		threadUUID := uuid.New()

		var (
			start = make(chan struct{})
			wg    sync.WaitGroup
		)

		for i := range writers {
			wg.Go(func() {
				payload, err := json.Marshal(&newsdoc.Document{
					Uuid:  uuid.NewString(),
					Type:  "core/comment",
					Title: fmt.Sprintf("Comment %d", i),
				})
				if err != nil {
					t.Errorf("round %d writer %d: marshal payload: %v", round, i, err)

					return
				}

				<-start

				_, thread, err := eu.Store.CreateComment(ctx, internal.Comment{
					UUID:       uuid.New(),
					ThreadUUID: threadUUID,
					Author:     fmt.Sprintf("core://user/writer-%d", i),
					Payload:    payload,
				}, docUUID, "se.ecms.user.comments.factcheck")
				if err != nil {
					t.Errorf("round %d writer %d: %v", round, i, err)

					return
				}

				if thread.UUID != threadUUID {
					t.Errorf("round %d writer %d: answered thread %s",
						round, i, thread.UUID)
				}
			})
		}

		close(start)
		wg.Wait()

		if t.Failed() {
			return
		}

		thread, err := eu.Store.GetThread(ctx, threadUUID)
		test.Mustf(t, err, "round %d: get the thread", round)

		test.Equalf(t, writers, len(thread.Comments),
			"round %d: every comment landed in the one thread", round)
		test.Equalf(t, int64(writers), thread.LiveComments,
			"round %d: live comments", round)
	}

	threads, err := eu.Store.ListThreads(ctx, docUUID, true)
	test.Mustf(t, err, "list threads")

	test.Equalf(t, rounds, len(threads), "one thread per round")
}

// TestConcurrentCommentPosts verifies that concurrent posts to one thread
// serialise on the thread row lock and all land. The post stamps its
// comment after taking the lock, so stamps follow commit order and the
// thread's updated time ends at the newest comment. Without the lock a post
// stamped earlier can touch the thread after one stamped later, leaving the
// thread older than its newest comment; that race is probabilistic and
// rarely hit, so the second half holds the lock from outside and checks
// deterministically that posts wait for it before stamping.
func TestConcurrentCommentPosts(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	const (
		writers        = 8
		postsPerWriter = 25
	)

	docUUID := uuid.New()
	threadUUID := uuid.New()

	payload := func(title string) []byte {
		data, err := json.Marshal(&newsdoc.Document{
			Uuid:  uuid.NewString(),
			Type:  "core/comment",
			Title: title,
		})
		test.Mustf(t, err, "marshal payload")

		return data
	}

	_, _, err := eu.Store.CreateComment(ctx, internal.Comment{
		UUID:       uuid.New(),
		ThreadUUID: threadUUID,
		Author:     "core://user/starter",
		Payload:    payload("Start"),
	}, docUUID, "")
	test.Mustf(t, err, "start the thread")

	var (
		start = make(chan struct{})
		wg    sync.WaitGroup
	)

	for w := range writers {
		payloads := make([][]byte, postsPerWriter)
		for i := range payloads {
			payloads[i] = payload(fmt.Sprintf("w%d-%d", w, i))
		}

		wg.Go(func() {
			<-start

			for i := range postsPerWriter {
				_, _, err := eu.Store.CreateComment(ctx, internal.Comment{
					UUID:       uuid.New(),
					ThreadUUID: threadUUID,
					Author:     fmt.Sprintf("core://user/writer-%d", w),
					Payload:    payloads[i],
				}, uuid.Nil, "")
				if err != nil {
					t.Errorf("writer %d post %d: %v", w, i, err)

					return
				}
			}
		})
	}

	close(start)
	wg.Wait()

	if t.Failed() {
		return
	}

	thread, err := eu.Store.GetThread(ctx, threadUUID)
	test.Mustf(t, err, "get the thread")

	want := writers*postsPerWriter + 1

	test.Equalf(t, want, len(thread.Comments), "every post landed")
	test.Equalf(t, int64(want), thread.LiveComments, "live comments")

	newest := thread.Comments[len(thread.Comments)-1].Created

	if !thread.Updated.Equal(newest) {
		t.Fatalf("thread updated %s is not the newest comment's created %s",
			thread.Updated.Format(time.RFC3339Nano),
			newest.Format(time.RFC3339Nano))
	}

	// Hold the thread row lock from outside the store while posts start.
	// Every post must wait for it before stamping its comment, so every
	// comment is created after the lock is released. A post that stamps
	// before taking the lock, or never takes it, is created while the
	// lock is still held.
	tx, err := eu.DB.Begin(ctx)
	test.Mustf(t, err, "begin the lock holder")

	defer func() {
		err := tx.Rollback(context.Background())
		if err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("roll back the lock holder: %v", err)
		}
	}()

	_, err = tx.Exec(ctx,
		"SELECT uuid FROM comment_thread WHERE uuid = $1 FOR UPDATE", threadUUID)
	test.Mustf(t, err, "lock the thread row")

	const held = 4

	var (
		heldWG  sync.WaitGroup
		created = make(chan time.Time, held)
	)

	for i := range held {
		data := payload(fmt.Sprintf("held-%d", i))

		heldWG.Go(func() {
			c, _, err := eu.Store.CreateComment(ctx, internal.Comment{
				UUID:       uuid.New(),
				ThreadUUID: threadUUID,
				Author:     "core://user/held",
				Payload:    data,
			}, uuid.Nil, "")
			if err != nil {
				t.Errorf("held post %d: %v", i, err)

				return
			}

			created <- c.Created
		})
	}

	// Let the posts reach the lock.
	time.Sleep(200 * time.Millisecond)

	released := time.Now()

	err = tx.Commit(ctx)
	test.Mustf(t, err, "release the thread row lock")

	heldWG.Wait()
	close(created)

	if t.Failed() {
		return
	}

	for c := range created {
		if !c.After(released) {
			t.Fatalf("a post was stamped at %s, before the lock was released at %s",
				c.Format(time.RFC3339Nano), released.Format(time.RFC3339Nano))
		}
	}
}

// TestConcurrentNotificationFanOut verifies that fan-outs to overlapping key
// sets all succeed and leave every key with dense ids in commit order. Each
// writer passes its keys in its own order; the fan-out locks them sorted, so
// two writers holding a key the other wants next cannot deadlock. Postgres
// would break such a deadlock by failing one of them, which shows up here
// as a writer error. Ids are checked against created, which is stamped
// after every key is locked and so follows id order within a key.
func TestConcurrentNotificationFanOut(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	const (
		writers         = 8
		eventsPerWriter = 25
	)

	keys := []string{
		"core://user/a",
		"core://user/b",
		"core://unit/c",
		"core://org/d",
	}

	// Writer w addresses three of the four keys, starting at a rotating
	// offset and walking backwards on odd writers, so every pair of
	// writers overlaps and they name the shared keys in different orders.
	writerKeys := func(w int) []string {
		var ks []string

		for i := range 3 {
			idx := (w + i) % len(keys)
			if w%2 == 1 {
				idx = (w + len(keys) - i) % len(keys)
			}

			ks = append(ks, keys[idx])
		}

		return ks
	}

	want := make(map[string]int)

	for w := range writers {
		for _, k := range writerKeys(w) {
			want[k] += eventsPerWriter
		}
	}

	var (
		start = make(chan struct{})
		wg    sync.WaitGroup
	)

	for w := range writers {
		wg.Go(func() {
			<-start

			for i := range eventsPerWriter {
				written, err := eu.Store.InsertNotifications(ctx,
					internal.NotificationSourceEvent{
						Family:       internal.NotificationFamilyIngest,
						Kind:         "se.ecms.local.test.updated",
						DocumentUUID: uuid.New(),
						DocumentType: "core/article",
						Actor:        fmt.Sprintf("core://user/writer-%d", w),
						EventUUID:    uuid.New(),
					}, writerKeys(w))
				if err != nil {
					t.Errorf("writer %d event %d: %v", w, i, err)

					return
				}

				if len(written) != 3 {
					t.Errorf("writer %d event %d: wrote %d rows, want 3",
						w, i, len(written))

					return
				}
			}
		})
	}

	close(start)
	wg.Wait()

	if t.Failed() {
		return
	}

	for _, key := range keys {
		rows, err := eu.Store.ListNotificationsAfterID(
			ctx, key, 0, time.Time{}, 1000)
		test.Mustf(t, err, "list rows under %s", key)

		test.Equalf(t, want[key], len(rows), "rows under %s", key)

		for i, r := range rows {
			if r.ID != int64(i+1) {
				t.Fatalf("%s: id %d at index %d, ids are not dense",
					key, r.ID, i)
			}

			if i > 0 && r.Created.Before(rows[i-1].Created) {
				t.Fatalf("%s: id %d created %s before id %d at %s",
					key, r.ID, r.Created.Format(time.RFC3339Nano),
					rows[i-1].ID, rows[i-1].Created.Format(time.RFC3339Nano))
			}
		}
	}

	counted := testutil.ToFloat64(eu.Metrics.NotificationsWritten.WithLabelValues(
		string(internal.NotificationFamilyIngest)))

	test.Equalf(t, float64(writers*eventsPerWriter*3), counted,
		"rows counted as written")
}

// TestConcurrentNotificationRepeat verifies that one event written under
// the same key by several writers at once, and again afterwards, leaves a
// single row: the unique (key, event_uuid) makes every write but the first
// a no-op.
func TestConcurrentNotificationRepeat(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	const (
		rounds  = 20
		writers = 6
	)

	key := "core://user/repeat"

	for round := range rounds {
		event := internal.NotificationSourceEvent{
			Family:       internal.NotificationFamilyIngest,
			Kind:         "se.ecms.local.test.updated",
			DocumentUUID: uuid.New(),
			DocumentType: "core/article",
			Actor:        "core://user/writer",
			EventUUID:    uuid.New(),
		}

		var (
			start   = make(chan struct{})
			wg      sync.WaitGroup
			mu      sync.Mutex
			winners int
		)

		for i := range writers {
			wg.Go(func() {
				<-start

				written, err := eu.Store.InsertNotifications(
					ctx, event, []string{key, key})
				if err != nil {
					t.Errorf("round %d writer %d: %v", round, i, err)

					return
				}

				mu.Lock()
				winners += len(written)
				mu.Unlock()
			})
		}

		close(start)
		wg.Wait()

		if t.Failed() {
			return
		}

		test.Equalf(t, 1, winners, "round %d: writes that wrote a row", round)

		written, err := eu.Store.InsertNotifications(ctx, event, []string{key})
		test.Mustf(t, err, "round %d: repeat the event", round)

		test.Equalf(t, 0, len(written), "round %d: rows written by the repeat", round)
	}

	rows, err := eu.Store.ListNotificationsAfterID(ctx, key, 0, time.Time{}, 1000)
	test.Mustf(t, err, "list rows")

	test.Equalf(t, rounds, len(rows), "one row per event")

	for i := 1; i < len(rows); i++ {
		if rows[i].ID <= rows[i-1].ID {
			t.Fatalf("ids not ascending at index %d: %d after %d",
				i, rows[i].ID, rows[i-1].ID)
		}
	}
}
