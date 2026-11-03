package internal

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrCommentThreadNotFound is returned when no thread has the given uuid.
var ErrCommentThreadNotFound = errors.New("comment thread not found")

// ErrCommentNotFound is returned when no comment has the given uuid.
var ErrCommentNotFound = errors.New("comment not found")

// ErrCommentConflict is returned when a comment with the same uuid is
// already stored but in another thread or with a different payload.
var ErrCommentConflict = errors.New(
	"a different comment with that uuid already exists")

// ErrCommentThreadConflict is returned when a comment is posted to a thread
// uuid that already exists on another document.
var ErrCommentThreadConflict = errors.New(
	"a thread with that uuid already exists on another document")

// ErrCommentDeleted is returned when an edit targets a deleted comment.
var ErrCommentDeleted = errors.New("comment has been deleted")

// ErrNotCommentAuthor is returned when the caller may not change a comment
// because they did not write it.
var ErrNotCommentAuthor = errors.New("not the author of the comment")

// CommentsStore is the storage the Comments handlers need.
type CommentsStore interface {
	OnCommentUpdate(
		ctx context.Context, ch chan CommentEvent,
		documentUUIDs []uuid.UUID,
	)
	GetCommentDocument(
		ctx context.Context, commentUUID uuid.UUID,
	) (uuid.UUID, error)
	CreateComment(
		ctx context.Context, comment Comment,
		documentUUID uuid.UUID, kind string,
	) (*Comment, *CommentThread, error)
	UpdateComment(
		ctx context.Context, commentUUID uuid.UUID,
		author string, payload json.RawMessage,
	) (*Comment, error)
	DeleteComment(
		ctx context.Context, commentUUID uuid.UUID,
		actor string, mayDeleteOthers bool,
	) error
	ResolveThread(
		ctx context.Context, threadUUID uuid.UUID, actor string,
	) (*CommentThread, error)
	ReopenThread(
		ctx context.Context, threadUUID uuid.UUID, actor string,
	) (*CommentThread, error)
	GetThread(
		ctx context.Context, threadUUID uuid.UUID,
	) (*CommentThread, error)
	ListThreads(
		ctx context.Context, documentUUID uuid.UUID,
		includeResolved bool,
	) ([]CommentThread, error)
	ListMentions(
		ctx context.Context, subject string,
		before time.Time, size int64,
	) ([]Mention, error)
}

// CommentThread is a thread of comments on a repository document. Resolved
// is nil while the thread is open. LiveComments counts the comments that
// are not deleted. Comments is populated by the reads that return them,
// oldest first.
type CommentThread struct {
	UUID         uuid.UUID
	DocumentUUID uuid.UUID
	Kind         string
	Created      time.Time
	CreatedBy    string
	Updated      time.Time
	Resolved     *time.Time
	ResolvedBy   string
	LiveComments int64
	Comments     []Comment
}

// Comment is a comment in a thread. Author is the writer's subject and
// AuthorName the name claim at write time. Deleted is nil while the comment
// is live; once it is set, Payload and Mentioned are empty on every read.
// Mentioned is derived from the payload's mention links, never taken from
// the caller. Payload is the JSON encoded core/comment document.
type Comment struct {
	UUID       uuid.UUID
	ThreadUUID uuid.UUID
	Author     string
	AuthorName string
	Created    time.Time
	Updated    time.Time
	Deleted    *time.Time
	DeletedBy  string
	Mentioned  []string
	Payload    json.RawMessage
}

// Mention is a reference to a comment that mentions a subject. It never
// carries the payload: the mention can be on a document the subject may
// not read, and the thread read is where access is checked.
type Mention struct {
	CommentUUID  uuid.UUID
	ThreadUUID   uuid.UUID
	DocumentUUID uuid.UUID
	Author       string
	AuthorName   string
	Created      time.Time
}

// CommentEventType is the kind of change a CommentEvent announces.
type CommentEventType string

const (
	CommentEventThreadCreated  CommentEventType = "thread_created"
	CommentEventCommentCreated CommentEventType = "comment_created"
	CommentEventCommentUpdated CommentEventType = "comment_updated"
	CommentEventCommentDeleted CommentEventType = "comment_deleted"
	CommentEventThreadResolved CommentEventType = "thread_resolved"
	CommentEventThreadReopened CommentEventType = "thread_reopened"
)

// CommentEvent announces a change to a thread or a comment on the
// comment_update channel. It is a wakeup: readers re-read, and the body is
// never carried. CommentUUID is nil for thread events, Mentioned is set on
// comment creates and updates only.
type CommentEvent struct {
	Type         CommentEventType `json:"type"`
	DocumentUUID uuid.UUID        `json:"document_uuid"`
	ThreadUUID   uuid.UUID        `json:"thread_uuid"`
	CommentUUID  *uuid.UUID       `json:"comment_uuid,omitempty"`
	Actor        string           `json:"actor"`
	Kind         string           `json:"kind"`
	Mentioned    []string         `json:"mentioned,omitempty"`
	Time         time.Time        `json:"time"`
}
