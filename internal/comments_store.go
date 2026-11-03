package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/ttab/elephant-user/postgres"
	"github.com/ttab/elephantine/pg"
)

// Interface guard.
var _ CommentsStore = &PGStore{}

// maxNotifyPayload is the largest payload Postgres accepts for a NOTIFY:
// it must be shorter than 8000 bytes in a default build.
const maxNotifyPayload = 7999

// OnCommentUpdate notifies ch of comment changes on any of the given
// documents. Subscription is cancelled with the context. Delivery is best
// effort: a non-blocking send on ch, so a busy receiver with an unbuffered
// channel misses events.
func (s *PGStore) OnCommentUpdate(
	ctx context.Context, ch chan CommentEvent,
	documentUUIDs []uuid.UUID,
) {
	docs := make(map[uuid.UUID]bool, len(documentUUIDs))
	for _, d := range documentUUIDs {
		docs[d] = true
	}

	go s.Comments.Listen(ctx, ch, func(e CommentEvent) bool {
		return docs[e.DocumentUUID]
	})
}

// CreateComment implements [CommentsStore]. It posts the comment to the
// thread comment.ThreadUUID, creating the thread on documentUUID with kind
// when it does not exist yet; for an existing thread documentUUID may be
// the nil uuid, otherwise it must match the thread's document. A missing
// thread without a documentUUID is ErrCommentThreadNotFound.
//
// The comment uuid makes the post idempotent: a stored comment with the
// same uuid, thread and payload is answered as if it had just been
// created, and one with another thread or payload is ErrCommentConflict.
// Posting to a resolved thread reopens it.
func (s *PGStore) CreateComment(
	ctx context.Context, comment Comment,
	documentUUID uuid.UUID, kind string,
) (*Comment, *CommentThread, error) {
	mentioned, err := mentionedSubjects(comment.Payload)
	if err != nil {
		return nil, nil, err
	}

	var (
		created *Comment
		thread  *CommentThread
	)

	err = pg.WithTX(ctx, s.dbpool, func(tx pgx.Tx) error {
		q := postgres.New(tx)

		// The thread row lock is the only lock the post takes. It
		// serialises posts to one thread, so the timestamp taken after
		// it orders comments in commit order.
		row, threadCreated, err := lockOrCreateThread(ctx, q,
			comment.ThreadUUID, documentUUID, kind, comment.Author)
		if err != nil {
			return err
		}

		if documentUUID != uuid.Nil && row.DocumentUuid != documentUUID {
			return ErrCommentThreadConflict
		}

		now := time.Now()

		inserted, err := q.InsertComment(ctx, postgres.InsertCommentParams{
			UUID:       comment.UUID,
			ThreadUuid: row.UUID,
			Author:     comment.Author,
			AuthorName: comment.AuthorName,
			Created:    pg.Time(now),
			Mentioned:  mentioned,
			Payload:    comment.Payload,
		})
		if err != nil {
			return fmt.Errorf("insert comment: %w", err)
		}

		if inserted == 0 {
			// A retry of a post whose response was lost, or a uuid
			// reused for something else. Nothing has been written.
			existing, err := existingComment(ctx, q, comment)
			if err != nil {
				return err
			}

			created = existing

			thread, err = getThread(ctx, q, row.UUID)

			return err
		}

		reopened := row.Resolved.Valid

		switch {
		case threadCreated:
			// Created with updated set to now, nothing to bump.
		case reopened:
			err = q.ReopenCommentThread(ctx, postgres.ReopenCommentThreadParams{
				Updated: pg.Time(now),
				UUID:    row.UUID,
			})
			if err != nil {
				return fmt.Errorf("reopen thread: %w", err)
			}
		default:
			err = q.TouchCommentThread(ctx, postgres.TouchCommentThreadParams{
				Updated: pg.Time(now),
				UUID:    row.UUID,
			})
			if err != nil {
				return fmt.Errorf("touch thread: %w", err)
			}
		}

		event := CommentEvent{
			DocumentUUID: row.DocumentUuid,
			ThreadUUID:   row.UUID,
			Actor:        comment.Author,
			Kind:         row.Kind,
			Time:         now,
		}

		if threadCreated {
			err = notifyCommentUpdated(ctx, q,
				event.withType(CommentEventThreadCreated))
			if err != nil {
				return err
			}
		}

		commentEvent := event.withType(CommentEventCommentCreated)
		commentEvent.CommentUUID = &comment.UUID
		commentEvent.Mentioned = mentioned

		err = notifyCommentUpdated(ctx, q, commentEvent)
		if err != nil {
			return err
		}

		if reopened {
			err = notifyCommentUpdated(ctx, q,
				event.withType(CommentEventThreadReopened))
			if err != nil {
				return err
			}
		}

		created, err = getComment(ctx, q, comment.UUID)
		if err != nil {
			return err
		}

		thread, err = getThread(ctx, q, row.UUID)

		return err
	})
	if err != nil {
		return nil, nil, err
	}

	return created, thread, nil
}

// lockOrCreateThread row-locks the thread, creating it first when it does
// not exist and documentUUID is set. Two posts racing to create one thread
// both succeed: the loser's insert waits for the winner to commit, does
// nothing, and the re-read under the lock finds the winner's thread. The
// returned flag reports whether this transaction created it.
func lockOrCreateThread(
	ctx context.Context, q *postgres.Queries,
	threadUUID uuid.UUID, documentUUID uuid.UUID,
	kind string, author string,
) (postgres.CommentThread, bool, error) {
	row, err := q.GetCommentThreadForUpdate(ctx, threadUUID)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Created below.
	case err != nil:
		return row, false, fmt.Errorf("lock thread: %w", err)
	default:
		return row, false, nil
	}

	if documentUUID == uuid.Nil {
		return row, false, ErrCommentThreadNotFound
	}

	inserted, err := q.InsertCommentThread(ctx, postgres.InsertCommentThreadParams{
		UUID:         threadUUID,
		DocumentUuid: documentUUID,
		Kind:         kind,
		Created:      pg.Time(time.Now()),
		CreatedBy:    author,
	})
	if err != nil {
		return row, false, fmt.Errorf("insert thread: %w", err)
	}

	row, err = q.GetCommentThreadForUpdate(ctx, threadUUID)
	if err != nil {
		return row, false, fmt.Errorf("lock created thread: %w", err)
	}

	return row, inserted == 1, nil
}

// existingComment answers a post whose comment uuid is already stored: the
// stored comment when it is in the same thread with an equal payload,
// otherwise ErrCommentConflict.
func existingComment(
	ctx context.Context, q *postgres.Queries, comment Comment,
) (*Comment, error) {
	stored, err := q.GetComment(ctx, comment.UUID)
	if err != nil {
		return nil, fmt.Errorf("get stored comment: %w", err)
	}

	if stored.ThreadUuid != comment.ThreadUUID {
		return nil, ErrCommentConflict
	}

	same, err := sameJSON(stored.Payload, comment.Payload)
	if err != nil {
		return nil, err
	}

	if !same {
		return nil, ErrCommentConflict
	}

	c := commentFromRow(stored)

	return &c, nil
}

// sameJSON reports whether two JSON documents are equal irrespective of
// whitespace and object key order, as jsonb equality is. The stored side
// comes back from jsonb in its normalised form.
func sameJSON(a []byte, b []byte) (bool, error) {
	ca, err := canonicalJSON(a)
	if err != nil {
		return false, fmt.Errorf("canonicalise stored payload: %w", err)
	}

	cb, err := canonicalJSON(b)
	if err != nil {
		return false, fmt.Errorf("canonicalise payload: %w", err)
	}

	return bytes.Equal(ca, cb), nil
}

func canonicalJSON(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	var v any

	err := dec.Decode(&v)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	// Marshal sorts object keys.
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}

	return out, nil
}

// UpdateComment implements [CommentsStore]. Only the author may replace
// the payload, otherwise ErrNotCommentAuthor; a deleted comment is
// ErrCommentDeleted. Mentions are derived again from the new payload.
func (s *PGStore) UpdateComment(
	ctx context.Context, commentUUID uuid.UUID,
	author string, payload json.RawMessage,
) (*Comment, error) {
	mentioned, err := mentionedSubjects(payload)
	if err != nil {
		return nil, err
	}

	var updated *Comment

	err = pg.WithTX(ctx, s.dbpool, func(tx pgx.Tx) error {
		q := postgres.New(tx)

		row, err := lockComment(ctx, q, commentUUID)
		if err != nil {
			return err
		}

		if row.Deleted.Valid {
			return ErrCommentDeleted
		}

		if row.Author != author {
			return ErrNotCommentAuthor
		}

		now := time.Now()

		err = q.UpdateComment(ctx, postgres.UpdateCommentParams{
			Payload:   payload,
			Mentioned: mentioned,
			Updated:   pg.Time(now),
			UUID:      commentUUID,
		})
		if err != nil {
			return fmt.Errorf("update comment: %w", err)
		}

		event, err := touchThreadForComment(ctx, q, row.ThreadUuid, now)
		if err != nil {
			return err
		}

		event.Type = CommentEventCommentUpdated
		event.CommentUUID = &commentUUID
		event.Actor = author
		event.Mentioned = mentioned

		err = notifyCommentUpdated(ctx, q, event)
		if err != nil {
			return err
		}

		updated, err = getComment(ctx, q, commentUUID)

		return err
	})
	if err != nil {
		return nil, err
	}

	return updated, nil
}

// DeleteComment implements [CommentsStore]. The author may delete their
// own comment, and an actor with mayDeleteOthers any comment; anyone else
// gets ErrNotCommentAuthor. The comment stays in its thread without its
// payload. Deleting a deleted comment is a successful no-op.
func (s *PGStore) DeleteComment(
	ctx context.Context, commentUUID uuid.UUID,
	actor string, mayDeleteOthers bool,
) error {
	return pg.WithTX(ctx, s.dbpool, func(tx pgx.Tx) error {
		q := postgres.New(tx)

		row, err := lockComment(ctx, q, commentUUID)
		if err != nil {
			return err
		}

		if row.Author != actor && !mayDeleteOthers {
			return ErrNotCommentAuthor
		}

		if row.Deleted.Valid {
			return nil
		}

		now := time.Now()

		err = q.DeleteComment(ctx, postgres.DeleteCommentParams{
			Deleted:   pg.Time(now),
			DeletedBy: pg.Text(actor),
			UUID:      commentUUID,
		})
		if err != nil {
			return fmt.Errorf("delete comment: %w", err)
		}

		event, err := touchThreadForComment(ctx, q, row.ThreadUuid, now)
		if err != nil {
			return err
		}

		event.Type = CommentEventCommentDeleted
		event.CommentUUID = &commentUUID
		event.Actor = actor

		return notifyCommentUpdated(ctx, q, event)
	})
}

// lockComment row-locks a comment for an edit or a delete.
func lockComment(
	ctx context.Context, q *postgres.Queries, commentUUID uuid.UUID,
) (postgres.Comment, error) {
	row, err := q.GetCommentForUpdate(ctx, commentUUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, ErrCommentNotFound
	}

	if err != nil {
		return row, fmt.Errorf("lock comment: %w", err)
	}

	return row, nil
}

// touchThreadForComment bumps the thread's updated time after a comment
// change and returns an event carrying the thread's document and kind.
func touchThreadForComment(
	ctx context.Context, q *postgres.Queries,
	threadUUID uuid.UUID, now time.Time,
) (CommentEvent, error) {
	err := q.TouchCommentThread(ctx, postgres.TouchCommentThreadParams{
		Updated: pg.Time(now),
		UUID:    threadUUID,
	})
	if err != nil {
		return CommentEvent{}, fmt.Errorf("touch thread: %w", err)
	}

	thread, err := q.GetCommentThread(ctx, threadUUID)
	if err != nil {
		return CommentEvent{}, fmt.Errorf("get thread: %w", err)
	}

	return CommentEvent{
		DocumentUUID: thread.DocumentUuid,
		ThreadUUID:   thread.UUID,
		Kind:         thread.Kind,
		Time:         now,
	}, nil
}

// ResolveThread implements [CommentsStore]. Resolving a resolved thread is
// a successful no-op and announces nothing.
func (s *PGStore) ResolveThread(
	ctx context.Context, threadUUID uuid.UUID, actor string,
) (*CommentThread, error) {
	return s.setThreadResolved(ctx, threadUUID, actor, true)
}

// ReopenThread implements [CommentsStore]. Reopening an open thread is a
// successful no-op and announces nothing.
func (s *PGStore) ReopenThread(
	ctx context.Context, threadUUID uuid.UUID, actor string,
) (*CommentThread, error) {
	return s.setThreadResolved(ctx, threadUUID, actor, false)
}

func (s *PGStore) setThreadResolved(
	ctx context.Context, threadUUID uuid.UUID,
	actor string, resolve bool,
) (*CommentThread, error) {
	var thread *CommentThread

	err := pg.WithTX(ctx, s.dbpool, func(tx pgx.Tx) error {
		q := postgres.New(tx)

		row, err := q.GetCommentThreadForUpdate(ctx, threadUUID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCommentThreadNotFound
		}

		if err != nil {
			return fmt.Errorf("lock thread: %w", err)
		}

		if row.Resolved.Valid != resolve {
			now := time.Now()
			eventType := CommentEventThreadResolved

			if resolve {
				err = q.ResolveCommentThread(ctx, postgres.ResolveCommentThreadParams{
					Resolved:   pg.Time(now),
					ResolvedBy: pg.Text(actor),
					UUID:       threadUUID,
				})
			} else {
				eventType = CommentEventThreadReopened

				err = q.ReopenCommentThread(ctx, postgres.ReopenCommentThreadParams{
					Updated: pg.Time(now),
					UUID:    threadUUID,
				})
			}

			if err != nil {
				return fmt.Errorf("update thread: %w", err)
			}

			err = notifyCommentUpdated(ctx, q, CommentEvent{
				Type:         eventType,
				DocumentUUID: row.DocumentUuid,
				ThreadUUID:   row.UUID,
				Actor:        actor,
				Kind:         row.Kind,
				Time:         now,
			})
			if err != nil {
				return err
			}
		}

		thread, err = getThread(ctx, q, threadUUID)

		return err
	})
	if err != nil {
		return nil, err
	}

	return thread, nil
}

// GetThread implements [CommentsStore]. The thread carries its comments
// oldest first, deleted ones included without their payload.
func (s *PGStore) GetThread(
	ctx context.Context, threadUUID uuid.UUID,
) (*CommentThread, error) {
	thread, err := getThread(ctx, s.q, threadUUID)
	if err != nil {
		return nil, err
	}

	rows, err := s.q.ListComments(ctx, threadUUID)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}

	thread.Comments = make([]Comment, len(rows))

	for i, r := range rows {
		thread.Comments[i] = commentFromRow(r)
	}

	return thread, nil
}

// ListThreads implements [CommentsStore]. Threads come most recently
// updated first, each with its comments oldest first; resolved threads are
// left out unless includeResolved is set.
func (s *PGStore) ListThreads(
	ctx context.Context, documentUUID uuid.UUID, includeResolved bool,
) ([]CommentThread, error) {
	rows, err := s.q.ListCommentThreads(ctx, postgres.ListCommentThreadsParams{
		DocumentUuid:    documentUUID,
		IncludeResolved: includeResolved,
	})
	if err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}

	threads := make([]CommentThread, len(rows))
	uuids := make([]uuid.UUID, len(rows))
	index := make(map[uuid.UUID]int, len(rows))

	for i, r := range rows {
		threads[i] = threadFromRow(postgres.GetCommentThreadRow(r))
		uuids[i] = r.UUID
		index[r.UUID] = i
	}

	if len(rows) == 0 {
		return threads, nil
	}

	comments, err := s.q.ListCommentsByThreads(ctx, uuids)
	if err != nil {
		return nil, fmt.Errorf("list comments: %w", err)
	}

	for _, c := range comments {
		i, ok := index[c.ThreadUuid]
		if !ok {
			continue
		}

		threads[i].Comments = append(threads[i].Comments, commentFromRow(c))
	}

	return threads, nil
}

// ListMentions implements [CommentsStore]. It lists references to the live
// comments that mention the subject, newest first, created before the
// given time; the zero time lists from the newest.
func (s *PGStore) ListMentions(
	ctx context.Context, subject string, before time.Time, size int64,
) ([]Mention, error) {
	rows, err := s.q.ListMentions(ctx, postgres.ListMentionsParams{
		Subject: subject,
		Before:  pg.TimeOrNull(before),
		Limit:   size,
	})
	if err != nil {
		return nil, fmt.Errorf("list mentions: %w", err)
	}

	mentions := make([]Mention, len(rows))

	for i, r := range rows {
		mentions[i] = Mention{
			CommentUUID:  r.UUID,
			ThreadUUID:   r.ThreadUuid,
			DocumentUUID: r.DocumentUuid,
			Author:       r.Author,
			AuthorName:   r.AuthorName,
			Created:      r.Created.Time,
		}
	}

	return mentions, nil
}

func getThread(
	ctx context.Context, q *postgres.Queries, threadUUID uuid.UUID,
) (*CommentThread, error) {
	row, err := q.GetCommentThread(ctx, threadUUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCommentThreadNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("get thread: %w", err)
	}

	thread := threadFromRow(row)

	return &thread, nil
}

func getComment(
	ctx context.Context, q *postgres.Queries, commentUUID uuid.UUID,
) (*Comment, error) {
	row, err := q.GetComment(ctx, commentUUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCommentNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("get comment: %w", err)
	}

	c := commentFromRow(row)

	return &c, nil
}

func threadFromRow(r postgres.GetCommentThreadRow) CommentThread {
	return CommentThread{
		UUID:         r.UUID,
		DocumentUUID: r.DocumentUuid,
		Kind:         r.Kind,
		Created:      r.Created.Time,
		CreatedBy:    r.CreatedBy,
		Updated:      r.Updated.Time,
		Resolved:     timePointer(r.Resolved),
		ResolvedBy:   r.ResolvedBy.String,
		LiveComments: r.LiveComments,
	}
}

// commentFromRow is the one place a comment row becomes a Comment, and
// where a deleted comment loses its payload and its mentions, which are
// part of what was deleted.
func commentFromRow(r postgres.Comment) Comment {
	c := Comment{
		UUID:       r.UUID,
		ThreadUUID: r.ThreadUuid,
		Author:     r.Author,
		AuthorName: r.AuthorName,
		Created:    r.Created.Time,
		Updated:    r.Updated.Time,
		Deleted:    timePointer(r.Deleted),
		DeletedBy:  r.DeletedBy.String,
		Mentioned:  r.Mentioned,
		Payload:    r.Payload,
	}

	if c.Deleted != nil {
		c.Mentioned = nil
		c.Payload = nil
	}

	return c
}

func timePointer(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}

	v := t.Time

	return &v
}

// commentPayload is the part of a core/comment document that mentions are
// read from.
type commentPayload struct {
	Content []struct {
		Links []struct {
			Rel string `json:"rel"`
			URI string `json:"uri"`
		} `json:"links"`
	} `json:"content"`
}

// mentionedSubjects derives the subjects a comment payload mentions: the
// uri of every link with rel "mention" in every content block,
// deduplicated and sorted. The handler has validated the payload, so this
// only extracts. The result is never nil, since the column is not null.
func mentionedSubjects(payload json.RawMessage) ([]string, error) {
	var doc commentPayload

	err := json.Unmarshal(payload, &doc)
	if err != nil {
		return nil, fmt.Errorf("decode comment payload: %w", err)
	}

	subjects := []string{}

	for _, block := range doc.Content {
		for _, link := range block.Links {
			if link.Rel != "mention" || link.URI == "" {
				continue
			}

			subjects = append(subjects, link.URI)
		}
	}

	slices.Sort(subjects)

	return slices.Compact(subjects), nil
}

func (e CommentEvent) withType(t CommentEventType) CommentEvent {
	e.Type = t

	return e
}

// notifyCommentUpdated publishes the event on comment_update. The payload
// must stay under the NOTIFY limit, and the mention list is the only part
// of it without a bound, so it is truncated to what fits. A listener loses
// nothing by that: the event is a wakeup and readers re-read the comment.
func notifyCommentUpdated(
	ctx context.Context, q *postgres.Queries, event CommentEvent,
) error {
	event, err := fitNotifyPayload(event)
	if err != nil {
		return err
	}

	err = pgNotify(ctx, q, NotifyChannelCommentUpdate, event)
	if err != nil {
		return fmt.Errorf("notify comment update: %w", err)
	}

	return nil
}

// fitNotifyPayload drops mentions from the end of the event's sorted list
// until its JSON encoding fits in a NOTIFY payload.
func fitNotifyPayload(event CommentEvent) (CommentEvent, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return event, fmt.Errorf("marshal comment event: %w", err)
	}

	if len(data) <= maxNotifyPayload {
		return event, nil
	}

	bare := event
	bare.Mentioned = nil

	data, err = json.Marshal(bare)
	if err != nil {
		return event, fmt.Errorf("marshal comment event: %w", err)
	}

	// Room for `,"mentioned":[]` and the entries with their commas.
	budget := maxNotifyPayload - len(data) - len(`,"mentioned":[]`)

	var kept []string

	for _, m := range event.Mentioned {
		entry, err := json.Marshal(m)
		if err != nil {
			return event, fmt.Errorf("marshal mention: %w", err)
		}

		cost := len(entry)
		if len(kept) > 0 {
			cost++
		}

		if cost > budget {
			break
		}

		kept = append(kept, m)
		budget -= cost
	}

	bare.Mentioned = kept

	return bare, nil
}
