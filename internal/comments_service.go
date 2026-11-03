package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	newsdoc_rpc "github.com/ttab/elephant-api/newsdoc"
	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephant-user/postgres"
	"github.com/ttab/elephantine/rpc"
	"github.com/ttab/newsdoc"
	"golang.org/x/net/html"
)

const (
	// waitDefaultSeconds is how long WaitForComments waits when the
	// request does not say.
	waitDefaultSeconds = 10
	// waitMaxSeconds caps how long WaitForComments waits.
	waitMaxSeconds = 30
	// waitMaxDocuments is how many documents one WaitForComments call may
	// wait for.
	waitMaxDocuments = 100
)

// CommentsService implements the Comments RPCs. Every RPC needs the user
// scope, and access to a thread follows the caller's permissions on the
// repository document it is on.
type CommentsService struct {
	store       CommentsStore
	validator   DocumentValidator
	permissions DocumentPermissions
}

func NewCommentsService(
	store CommentsStore, validator DocumentValidator,
	permissions DocumentPermissions,
) *CommentsService {
	return &CommentsService{
		store:       store,
		validator:   validator,
		permissions: permissions,
	}
}

// Interface guard.
var _ user.Comments = &CommentsService{}

// documentAccess is the permission a handler needs on a document.
type documentAccess string

const (
	accessRead  documentAccess = "read"
	accessWrite documentAccess = "write"
)

// requireDocumentAccess checks that the caller has the access on the
// document: permission_denied when the repository does not grant it,
// not_found for a document the repository does not know.
func (s *CommentsService) requireDocumentAccess(
	ctx context.Context, documentUUID uuid.UUID, access documentAccess,
) error {
	set, err := s.permissions.Permissions(ctx, documentUUID.String())
	if errors.Is(err, ErrDocumentNotFound) {
		return rpc.NotFound(fmt.Sprintf(
			"no such document %s", documentUUID))
	}

	if err != nil {
		return rpc.Internalf(
			"check %s permission on document %s: %w",
			access, documentUUID, err)
	}

	granted := set.Read
	if access == accessWrite {
		granted = set.Write
	}

	if !granted {
		return rpc.PermissionDeniedf(
			"no %s permission on document %s", access, documentUUID)
	}

	return nil
}

// parseUUID parses a required uuid argument.
func parseUUID(argument string, value string) (uuid.UUID, error) {
	if value == "" {
		return uuid.Nil, rpc.RequiredArgument(argument)
	}

	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, rpc.InvalidArgument(argument, err.Error())
	}

	return id, nil
}

// validateCommentPayload validates a comment body against the comments
// schemas and checks the inline id bindings that revisor cannot express,
// and returns the payload as stored.
func (s *CommentsService) validateCommentPayload(
	ctx context.Context, payload *newsdoc_rpc.Document,
) (json.RawMessage, error) {
	if payload == nil {
		return nil, rpc.RequiredArgument("payload")
	}

	doc := newsdoc_rpc.DocumentFromRPC(payload)

	validationResult, err := s.validator.ValidateDocument(
		ctx, postgres.SchemaUsageComments, &doc)
	if err != nil {
		return nil, rpc.Internalf("validate comment payload: %w", err)
	}

	if len(validationResult) > 0 {
		return nil, validationError(validationResult)
	}

	err = checkInlineLinkIDs(doc)
	if err != nil {
		return nil, rpc.InvalidArgument("payload", err.Error())
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, rpc.Internalf("marshal comment payload: %w", err)
	}

	return data, nil
}

// Link rels in a comment's text blocks.
const (
	linkRelMention = "mention"
	linkRelSeeAlso = "see-also"
)

// inlineLinkRels maps the inline elements of a comment's html that refer
// to a link to the rel of the link they must be bound to.
var inlineLinkRels = map[string]string{
	"mention": linkRelMention,
	"a":       linkRelSeeAlso,
}

// checkInlineLinkIDs checks that every <mention id="..."> and <a id="...">
// element in a content block's text has a link in the same block with the
// same id and the matching rel: mention for <mention>, see-also for <a>.
// Revisor validates the html against the policy but cannot bind an element
// to a link. An element without an id is the schema's to refuse and is
// skipped here. The html is tokenised the way revisor reads it.
func checkInlineLinkIDs(doc newsdoc.Document) error {
	for i, block := range doc.Content {
		text := block.Data["text"]
		if text == "" {
			continue
		}

		links := make(map[string]string, len(block.Links))

		for _, link := range block.Links {
			if link.ID != "" {
				links[link.ID] = link.Rel
			}
		}

		z := html.NewTokenizer(strings.NewReader(text))

		for {
			tt := z.Next()
			if tt == html.ErrorToken {
				break
			}

			if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
				continue
			}

			tok := z.Token()

			wantRel, ok := inlineLinkRels[tok.Data]
			if !ok {
				continue
			}

			id := elementID(tok)
			if id == "" {
				continue
			}

			rel, ok := links[id]
			if !ok {
				return fmt.Errorf(
					"content block %d: <%s id=%q> has no link with that id",
					i, tok.Data, id)
			}

			if rel != wantRel {
				return fmt.Errorf(
					"content block %d: <%s id=%q> is bound to a %q link, expected %q",
					i, tok.Data, id, rel, wantRel)
			}
		}
	}

	return nil
}

func elementID(tok html.Token) string {
	for _, attr := range tok.Attr {
		if attr.Key == "id" {
			return attr.Val
		}
	}

	return ""
}

// CreateComment implements [user.Comments].
func (s *CommentsService) CreateComment(
	ctx context.Context, req *user.CreateCommentRequest,
) (*user.CreateCommentResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	commentUUID, err := parseUUID("uuid", req.Uuid)
	if err != nil {
		return nil, err
	}

	threadUUID, err := parseUUID("thread_uuid", req.ThreadUuid)
	if err != nil {
		return nil, err
	}

	var requestedDoc uuid.UUID

	if req.DocumentUuid != "" {
		requestedDoc, err = parseUUID("document_uuid", req.DocumentUuid)
		if err != nil {
			return nil, err
		}
	}

	// An existing thread decides the document; a new one takes it from
	// the request.
	documentUUID := requestedDoc

	thread, err := s.store.GetThread(ctx, threadUUID)

	switch {
	case errors.Is(err, ErrCommentThreadNotFound):
		if requestedDoc == uuid.Nil {
			return nil, newThreadNeedsDocumentError()
		}
	case err != nil:
		return nil, rpc.Internalf("get thread: %w", err)
	default:
		if requestedDoc != uuid.Nil && requestedDoc != thread.DocumentUUID {
			return nil, threadConflictError()
		}

		documentUUID = thread.DocumentUUID
	}

	err = s.requireDocumentAccess(ctx, documentUUID, accessWrite)
	if err != nil {
		return nil, err
	}

	payload, err := s.validateCommentPayload(ctx, req.Payload)
	if err != nil {
		return nil, err
	}

	// The store is handed the document access was checked on, so a thread
	// created on another document in the meantime is a conflict rather
	// than a comment on a document the caller was not checked against.
	comment, created, err := s.store.CreateComment(ctx, Comment{
		UUID:       commentUUID,
		ThreadUUID: threadUUID,
		Author:     auth.Claims.Subject,
		AuthorName: auth.Claims.Name,
		Payload:    payload,
	}, documentUUID, req.Kind)

	switch {
	case errors.Is(err, ErrCommentThreadNotFound):
		return nil, newThreadNeedsDocumentError()
	case errors.Is(err, ErrCommentThreadConflict):
		return nil, threadConflictError()
	case errors.Is(err, ErrCommentConflict):
		return nil, rpc.AlreadyExists(fmt.Sprintf(
			"a different comment with uuid %s already exists", commentUUID))
	case err != nil:
		return nil, rpc.Internalf("create comment: %w", err)
	}

	rpcComment, err := commentToRPC(*comment)
	if err != nil {
		return nil, rpc.Internalf("create comment: %w", err)
	}

	rpcThread, err := threadToRPC(*created, false)
	if err != nil {
		return nil, rpc.Internalf("create comment: %w", err)
	}

	return &user.CreateCommentResponse{
		Comment: rpcComment,
		Thread:  rpcThread,
	}, nil
}

func newThreadNeedsDocumentError() error {
	return rpc.InvalidArgument("document_uuid",
		"the thread does not exist, and a new thread needs a document_uuid")
}

func threadConflictError() error {
	return rpc.InvalidArgument("document_uuid",
		"the thread exists on another document")
}

// commentDocument resolves the document a comment is on, for the access
// check in front of an edit or a delete.
func (s *CommentsService) commentDocument(
	ctx context.Context, commentUUID uuid.UUID,
) (uuid.UUID, error) {
	documentUUID, err := s.store.GetCommentDocument(ctx, commentUUID)
	if errors.Is(err, ErrCommentNotFound) {
		return uuid.Nil, rpc.NotFound("no such comment")
	}

	if err != nil {
		return uuid.Nil, rpc.Internalf("get comment: %w", err)
	}

	return documentUUID, nil
}

// UpdateComment implements [user.Comments].
func (s *CommentsService) UpdateComment(
	ctx context.Context, req *user.UpdateCommentRequest,
) (*user.UpdateCommentResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	commentUUID, err := parseUUID("uuid", req.Uuid)
	if err != nil {
		return nil, err
	}

	documentUUID, err := s.commentDocument(ctx, commentUUID)
	if err != nil {
		return nil, err
	}

	err = s.requireDocumentAccess(ctx, documentUUID, accessWrite)
	if err != nil {
		return nil, err
	}

	payload, err := s.validateCommentPayload(ctx, req.Payload)
	if err != nil {
		return nil, err
	}

	comment, err := s.store.UpdateComment(
		ctx, commentUUID, auth.Claims.Subject, payload)

	switch {
	case errors.Is(err, ErrCommentNotFound):
		return nil, rpc.NotFound("no such comment")
	case errors.Is(err, ErrCommentDeleted):
		return nil, rpc.FailedPreconditionf("the comment has been deleted")
	case errors.Is(err, ErrNotCommentAuthor):
		return nil, rpc.PermissionDeniedf(
			"only the author can edit a comment")
	case err != nil:
		return nil, rpc.Internalf("update comment: %w", err)
	}

	rpcComment, err := commentToRPC(*comment)
	if err != nil {
		return nil, rpc.Internalf("update comment: %w", err)
	}

	return &user.UpdateCommentResponse{Comment: rpcComment}, nil
}

// DeleteComment implements [user.Comments].
func (s *CommentsService) DeleteComment(
	ctx context.Context, req *user.DeleteCommentRequest,
) (*user.DeleteCommentResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	commentUUID, err := parseUUID("uuid", req.Uuid)
	if err != nil {
		return nil, err
	}

	documentUUID, err := s.commentDocument(ctx, commentUUID)
	if err != nil {
		return nil, err
	}

	err = s.requireDocumentAccess(ctx, documentUUID, accessWrite)
	if err != nil {
		return nil, err
	}

	err = s.store.DeleteComment(ctx, commentUUID, auth.Claims.Subject,
		auth.Claims.HasScope(ScopeDocAdmin))

	switch {
	case errors.Is(err, ErrCommentNotFound):
		return nil, rpc.NotFound("no such comment")
	case errors.Is(err, ErrNotCommentAuthor):
		return nil, rpc.PermissionDeniedf(
			"only the author or an admin can delete a comment")
	case err != nil:
		return nil, rpc.Internalf("delete comment: %w", err)
	}

	return &user.DeleteCommentResponse{}, nil
}

// GetThread implements [user.Comments].
func (s *CommentsService) GetThread(
	ctx context.Context, req *user.GetThreadRequest,
) (*user.GetThreadResponse, error) {
	_, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	threadUUID, err := parseUUID("uuid", req.Uuid)
	if err != nil {
		return nil, err
	}

	thread, err := s.store.GetThread(ctx, threadUUID)
	if errors.Is(err, ErrCommentThreadNotFound) {
		return nil, rpc.NotFound("no such thread")
	}

	if err != nil {
		return nil, rpc.Internalf("get thread: %w", err)
	}

	err = s.requireDocumentAccess(ctx, thread.DocumentUUID, accessRead)
	if err != nil {
		return nil, err
	}

	rpcThread, err := threadToRPC(*thread, true)
	if err != nil {
		return nil, rpc.Internalf("get thread: %w", err)
	}

	return &user.GetThreadResponse{Thread: rpcThread}, nil
}

// ListThreads implements [user.Comments].
func (s *CommentsService) ListThreads(
	ctx context.Context, req *user.ListThreadsRequest,
) (*user.ListThreadsResponse, error) {
	_, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	documentUUID, err := parseUUID("document_uuid", req.DocumentUuid)
	if err != nil {
		return nil, err
	}

	err = s.requireDocumentAccess(ctx, documentUUID, accessRead)
	if err != nil {
		return nil, err
	}

	threads, err := s.store.ListThreads(ctx, documentUUID, req.IncludeResolved)
	if err != nil {
		return nil, rpc.Internalf("list threads: %w", err)
	}

	res := user.ListThreadsResponse{
		Threads: make([]*user.CommentThread, len(threads)),
	}

	for i := range threads {
		t, err := threadToRPC(threads[i], true)
		if err != nil {
			return nil, rpc.Internalf("list threads: %w", err)
		}

		res.Threads[i] = t
	}

	return &res, nil
}

// ResolveThread implements [user.Comments].
func (s *CommentsService) ResolveThread(
	ctx context.Context, req *user.ResolveThreadRequest,
) (*user.ResolveThreadResponse, error) {
	thread, err := s.setThreadResolved(ctx, req.Uuid, true)
	if err != nil {
		return nil, err
	}

	return &user.ResolveThreadResponse{Thread: thread}, nil
}

// ReopenThread implements [user.Comments].
func (s *CommentsService) ReopenThread(
	ctx context.Context, req *user.ReopenThreadRequest,
) (*user.ReopenThreadResponse, error) {
	thread, err := s.setThreadResolved(ctx, req.Uuid, false)
	if err != nil {
		return nil, err
	}

	return &user.ReopenThreadResponse{Thread: thread}, nil
}

func (s *CommentsService) setThreadResolved(
	ctx context.Context, rawUUID string, resolve bool,
) (*user.CommentThread, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	threadUUID, err := parseUUID("uuid", rawUUID)
	if err != nil {
		return nil, err
	}

	current, err := s.store.GetThread(ctx, threadUUID)
	if errors.Is(err, ErrCommentThreadNotFound) {
		return nil, rpc.NotFound("no such thread")
	}

	if err != nil {
		return nil, rpc.Internalf("get thread: %w", err)
	}

	err = s.requireDocumentAccess(ctx, current.DocumentUUID, accessWrite)
	if err != nil {
		return nil, err
	}

	action := s.store.ReopenThread
	if resolve {
		action = s.store.ResolveThread
	}

	thread, err := action(ctx, threadUUID, auth.Claims.Subject)
	if errors.Is(err, ErrCommentThreadNotFound) {
		return nil, rpc.NotFound("no such thread")
	}

	if err != nil {
		return nil, rpc.Internalf("update thread: %w", err)
	}

	rpcThread, err := threadToRPC(*thread, false)
	if err != nil {
		return nil, rpc.Internalf("update thread: %w", err)
	}

	return rpcThread, nil
}

// ListMentions implements [user.Comments].
func (s *CommentsService) ListMentions(
	ctx context.Context, req *user.ListMentionsRequest,
) (*user.ListMentionsResponse, error) {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	if req.Size < 0 {
		return nil, rpc.InvalidArgument("size", "cannot be negative")
	}

	var before time.Time

	if req.Before != "" {
		before, err = time.Parse(time.RFC3339, req.Before)
		if err != nil {
			return nil, rpc.InvalidArgument("before", err.Error())
		}
	}

	mentions, err := s.store.ListMentions(
		ctx, auth.Claims.Subject, before, clampSize(req.Size))
	if err != nil {
		return nil, rpc.Internalf("list mentions: %w", err)
	}

	res := user.ListMentionsResponse{
		Mentions: make([]*user.Mention, len(mentions)),
	}

	for i, m := range mentions {
		res.Mentions[i] = &user.Mention{
			CommentUuid:  m.CommentUUID.String(),
			ThreadUuid:   m.ThreadUUID.String(),
			DocumentUuid: m.DocumentUUID.String(),
			Author:       m.Author,
			AuthorName:   m.AuthorName,
			Created:      formatCommentTime(m.Created),
		}
	}

	return &res, nil
}

// WaitForComments implements [user.Comments]. It answers on the first
// change to any of the documents, with that document and any other whose
// event had already arrived by then; it does not linger to collect more.
// The event is a wakeup only: nothing but its document uuid is used.
func (s *CommentsService) WaitForComments(
	ctx context.Context, req *user.WaitForCommentsRequest,
) (*user.WaitForCommentsResponse, error) {
	_, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return nil, err
	}

	if len(req.DocumentUuids) == 0 {
		return nil, rpc.RequiredArgument("document_uuids")
	}

	if len(req.DocumentUuids) > waitMaxDocuments {
		return nil, rpc.InvalidArgumentf("document_uuids",
			"at most %d documents, got %d",
			waitMaxDocuments, len(req.DocumentUuids))
	}

	if req.MaxWaitSec < 0 {
		return nil, rpc.InvalidArgument("max_wait_sec", "cannot be negative")
	}

	maxWait := req.MaxWaitSec
	if maxWait == 0 {
		maxWait = waitDefaultSeconds
	}

	maxWait = min(maxWait, waitMaxSeconds)

	documents := make([]uuid.UUID, 0, len(req.DocumentUuids))

	for i, raw := range req.DocumentUuids {
		id, err := parseUUID(fmt.Sprintf("document_uuids.%d", i), raw)
		if err != nil {
			return nil, err
		}

		if slices.Contains(documents, id) {
			continue
		}

		documents = append(documents, id)
	}

	for _, id := range documents {
		err := s.requireDocumentAccess(ctx, id, accessRead)
		if err != nil {
			return nil, err
		}
	}

	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Delivery is a non-blocking send, so the buffer is what keeps the
	// events that arrive while the first one is being handled. One write
	// emits up to two events for its document.
	events := make(chan CommentEvent, 4*len(documents))

	s.store.OnCommentUpdate(waitCtx, events, documents)

	timer := time.NewTimer(time.Duration(maxWait) * time.Second)
	defer timer.Stop()

	var first CommentEvent

	select {
	case first = <-events:
	case <-timer.C:
		return &user.WaitForCommentsResponse{}, nil
	case <-ctx.Done():
		return nil, waitEndedError(ctx)
	}

	changed := []string{first.DocumentUUID.String()}

drain:
	for {
		select {
		case e := <-events:
			id := e.DocumentUUID.String()
			if !slices.Contains(changed, id) {
				changed = append(changed, id)
			}
		default:
			break drain
		}
	}

	return &user.WaitForCommentsResponse{DocumentUuids: changed}, nil
}

// formatCommentTime formats a comment timestamp as RFC3339 with its
// fractional seconds: ListMentions pages on the created timestamp, and an
// edit within the second of the write must still read as later.
func formatCommentTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func optionalCommentTime(t *time.Time) string {
	if t == nil {
		return ""
	}

	return formatCommentTime(*t)
}

func commentToRPC(c Comment) (*user.Comment, error) {
	res := user.Comment{
		Uuid:       c.UUID.String(),
		ThreadUuid: c.ThreadUUID.String(),
		Author:     c.Author,
		AuthorName: c.AuthorName,
		Created:    formatCommentTime(c.Created),
		Updated:    formatCommentTime(c.Updated),
		Deleted:    optionalCommentTime(c.Deleted),
		DeletedBy:  c.DeletedBy,
		Mentioned:  c.Mentioned,
	}

	// A deleted comment comes from the store without a payload.
	if c.Deleted == nil && len(c.Payload) > 0 {
		payload, err := unmarshalDocument(c.Payload)
		if err != nil {
			return nil, fmt.Errorf("comment %s: %w", c.UUID, err)
		}

		res.Payload = payload
	}

	return &res, nil
}

// threadToRPC maps a thread, with its comments when withComments is set.
// The responses to writes carry the thread without its comments.
func threadToRPC(t CommentThread, withComments bool) (*user.CommentThread, error) {
	res := user.CommentThread{
		Uuid:         t.UUID.String(),
		DocumentUuid: t.DocumentUUID.String(),
		Kind:         t.Kind,
		Created:      formatCommentTime(t.Created),
		CreatedBy:    t.CreatedBy,
		Updated:      formatCommentTime(t.Updated),
		Resolved:     optionalCommentTime(t.Resolved),
		ResolvedBy:   t.ResolvedBy,
		LiveComments: t.LiveComments,
	}

	if !withComments {
		return &res, nil
	}

	res.Comments = make([]*user.Comment, len(t.Comments))

	for i := range t.Comments {
		c, err := commentToRPC(t.Comments[i])
		if err != nil {
			return nil, err
		}

		res.Comments[i] = c
	}

	return &res, nil
}
