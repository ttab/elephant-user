package internal_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"github.com/ttab/elephant-api/newsdoc"
	"github.com/ttab/elephant-api/repository"
	"github.com/ttab/elephant-api/repository/repositoryconnect"
	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/rpc"
	"github.com/ttab/elephantine/test"
)

// repositoryStub stands in for elephant-repository's Documents service in
// the test server. It answers GetPermissions for the subject of the
// forwarded token: read and write on every document unless a test has set
// something else for the document and subject, or marked the document as
// unknown.
type repositoryStub struct {
	repositoryconnect.UnimplementedDocumentsHandler

	Client *http.Client
	URL    string

	mu       sync.Mutex
	grants   map[repositoryGrantKey][]string
	notFound map[string]bool
}

type repositoryGrantKey struct {
	Document string
	Subject  string
}

func newRepositoryStub(t *testing.T) *repositoryStub {
	t.Helper()

	stub := repositoryStub{
		grants:   make(map[repositoryGrantKey][]string),
		notFound: make(map[string]bool),
	}

	path, handler := repositoryconnect.NewDocumentsHandler(&stub)

	mux := http.NewServeMux()
	mux.Handle(path, handler)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	stub.Client = server.Client()
	stub.URL = server.URL

	return &stub
}

// Grant sets the permission keys ("r", "w", "m") the subject has on the
// document; none means nothing is granted.
func (s *repositoryStub) Grant(document string, subject string, keys ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.grants[repositoryGrantKey{Document: document, Subject: subject}] = keys
}

// NotFound makes the repository answer not_found for the document.
func (s *repositoryStub) NotFound(document string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.notFound[document] = true
}

// GetPermissions implements repositoryconnect.DocumentsHandler.
func (s *repositoryStub) GetPermissions(
	_ context.Context, req *connect.Request[repository.GetPermissionsRequest],
) (*connect.Response[repository.GetPermissionsResponse], error) {
	subject, err := forwardedSubject(req.Header().Get("Authorization"))
	if err != nil {
		return nil, rpc.Errorf(connect.CodeUnauthenticated, "%v", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.notFound[req.Msg.Uuid] {
		return nil, rpc.NotFound("no document " + req.Msg.Uuid)
	}

	keys, ok := s.grants[repositoryGrantKey{
		Document: req.Msg.Uuid,
		Subject:  subject,
	}]
	if !ok {
		keys = []string{"r", "w"}
	}

	perms := make(map[string]string, len(keys))
	for _, k := range keys {
		perms[k] = subject
	}

	return connect.NewResponse(&repository.GetPermissionsResponse{
		Permissions: perms,
	}), nil
}

// forwardedSubject reads the subject from the bearer token the service
// forwarded, the way the repository would see it: core://user/<sub>.
func forwardedSubject(authorization string) (string, error) {
	token, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok {
		return "", fmt.Errorf("no bearer token forwarded")
	}

	var claims elephantine.JWTClaims

	_, _, err := jwt.NewParser().ParseUnverified(token, &claims)
	if err != nil {
		return "", fmt.Errorf("parse forwarded token: %w", err)
	}

	return "core://user/" + claims.Subject, nil
}

func commentClaims(subject string, scope string, name string) elephantine.JWTClaims {
	claims := userClaims(subject, scope)
	claims.Name = name

	return claims
}

// rpcCommentPayload builds a core/comment body with one text block, and a
// mention link with the id m<i> for each subject. The text carries the
// matching inline mention elements.
func rpcCommentPayload(text string, mentions ...string) *newsdoc.Document {
	block := newsdoc.Block{
		Type: "core/text",
		Data: map[string]string{},
	}

	for i, subject := range mentions {
		id := fmt.Sprintf("m%d", i)

		text += fmt.Sprintf(` <mention id="%s">%s</mention>`, id, subject)

		block.Links = append(block.Links, &newsdoc.Block{
			Id:   id,
			Rel:  "mention",
			Type: "tt/keycloak",
			Uri:  subject,
		})
	}

	block.Data["text"] = text

	return &newsdoc.Document{
		Uuid:    "6f1d1d8e-2b1a-4c55-9d7e-3a2f4b5c6d7e",
		Type:    "core/comment",
		Content: []*newsdoc.Block{&block},
	}
}

func commentUUIDs(comments []*user.Comment) []string {
	out := make([]string, len(comments))
	for i, c := range comments {
		out[i] = c.Uuid
	}

	return out
}

func threadUUIDs(threads []*user.CommentThread) []string {
	out := make([]string, len(threads))
	for i, c := range threads {
		out[i] = c.Uuid
	}

	return out
}

func expectStrings(t *testing.T, want []string, got []string, msg string) {
	t.Helper()

	if !slices.Equal(want, got) {
		t.Fatalf("%s: got %v, want %v", msg, got, want)
	}
}

func parseRFC3339(t *testing.T, value string) time.Time {
	t.Helper()

	v, err := time.Parse(time.RFC3339, value)
	test.Mustf(t, err, "parse timestamp %q", value)

	return v
}

// TestCommentsService runs the Comments RPCs over the wire: creating
// threads and comments, idempotent repeats, validation, authorship rules
// for edits and deletes, resolve and reopen, the reads, and the document
// permission checks against the repository stub.
func TestCommentsService(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	const (
		docA     = "a0000000-0000-4000-8000-000000000001"
		docB     = "a0000000-0000-4000-8000-000000000002"
		docOther = "a0000000-0000-4000-8000-000000000003"

		threadA1 = "b0000000-0000-4000-8000-000000000001"
		threadA2 = "b0000000-0000-4000-8000-000000000002"
		threadB1 = "b0000000-0000-4000-8000-000000000003"

		comment1 = "c0000000-0000-4000-8000-000000000001"
		comment2 = "c0000000-0000-4000-8000-000000000002"
		comment3 = "c0000000-0000-4000-8000-000000000003"
		comment4 = "c0000000-0000-4000-8000-000000000004"
		comment5 = "c0000000-0000-4000-8000-000000000005"

		alice = "core://user/alice"
		bob   = "core://user/bob"
		anna  = "core://user/anna"
		admin = "core://user/admin"
	)

	aliceCtx := bearerContext(ctx, eu.AccessToken(t,
		commentClaims("alice", "user", "Alice Andersson")))
	bobCtx := bearerContext(ctx, eu.AccessToken(t,
		commentClaims("bob", "user", "")))
	adminCtx := bearerContext(ctx, eu.AccessToken(t,
		commentClaims("admin", "user doc_admin", "Admin")))
	noScopeCtx := bearerContext(ctx, eu.AccessToken(t,
		commentClaims("alice", "", "Alice Andersson")))

	create := func(
		ctx context.Context, id, thread, document string,
		payload *newsdoc.Document,
	) (*user.CreateCommentResponse, error) {
		return eu.Comments.CreateComment(ctx, &user.CreateCommentRequest{
			Uuid:         id,
			ThreadUuid:   thread,
			DocumentUuid: document,
			Kind:         "factcheck",
			Payload:      payload,
		})
	}

	getThread := func(ctx context.Context, id string) *user.CommentThread {
		t.Helper()

		res, err := eu.Comments.GetThread(ctx, &user.GetThreadRequest{Uuid: id})
		test.Mustf(t, err, "get thread %s", id)

		return res.Thread
	}

	t.Run("create a thread", func(t *testing.T) {
		res, err := create(aliceCtx, comment1, threadA1, docA,
			rpcCommentPayload("Check this with", anna))
		test.Mustf(t, err, "create the first comment")

		test.Equalf(t, comment1, res.Comment.Uuid, "comment uuid")
		test.Equalf(t, threadA1, res.Comment.ThreadUuid, "comment thread")
		test.Equalf(t, alice, res.Comment.Author, "author from the token")
		test.Equalf(t, "Alice Andersson", res.Comment.AuthorName,
			"author name from the sub_name claim")
		expectStrings(t, []string{anna}, res.Comment.Mentioned,
			"mentions derived from the payload")
		test.Equalf(t, "core/comment", res.Comment.Payload.GetType(),
			"payload returned")
		test.Equalf(t, "", res.Comment.Deleted, "live comment")

		test.Equalf(t, threadA1, res.Thread.Uuid, "thread uuid")
		test.Equalf(t, docA, res.Thread.DocumentUuid, "thread document")
		test.Equalf(t, "factcheck", res.Thread.Kind, "thread kind")
		test.Equalf(t, alice, res.Thread.CreatedBy, "thread creator")
		test.Equalf(t, int64(1), res.Thread.LiveComments, "live comments")
		test.Equalf(t, 0, len(res.Thread.Comments),
			"create answers with the thread without its comments")
		test.Equalf(t, "", res.Thread.Resolved, "open thread")
	})

	t.Run("idempotent repeat", func(t *testing.T) {
		first := getThread(aliceCtx, threadA1)

		res, err := create(aliceCtx, comment1, threadA1, docA,
			rpcCommentPayload("Check this with", anna))
		test.Mustf(t, err, "repeat the first comment")

		test.Equalf(t, comment1, res.Comment.Uuid, "same comment")
		test.Equalf(t, first.Comments[0].Created, res.Comment.Created,
			"answer with the stored comment")
		test.Equalf(t, int64(1), res.Thread.LiveComments,
			"store nothing for a repeat")
	})

	t.Run("repeat with another payload", func(t *testing.T) {
		_, err := create(aliceCtx, comment1, threadA1, docA,
			rpcCommentPayload("Something else"))
		test.IsRPCError(t, err, connect.CodeAlreadyExists)
	})

	t.Run("create on an existing thread", func(t *testing.T) {
		res, err := create(bobCtx, comment2, threadA1, "",
			rpcCommentPayload("Done."))
		test.Mustf(t, err, "answer in the thread")

		test.Equalf(t, bob, res.Comment.Author, "author from the token")
		test.Equalf(t, "", res.Comment.AuthorName,
			"no name without a sub_name claim")
		test.Equalf(t, 0, len(res.Comment.Mentioned), "no mentions")
		test.Equalf(t, docA, res.Thread.DocumentUuid,
			"thread keeps its document")
		test.Equalf(t, int64(2), res.Thread.LiveComments, "live comments")
	})

	t.Run("existing thread on another document", func(t *testing.T) {
		_, err := create(bobCtx, comment3, threadA1, docB,
			rpcCommentPayload("Wrong document."))
		test.IsRPCError(t, err, connect.CodeInvalidArgument)
	})

	t.Run("new thread without a document", func(t *testing.T) {
		_, err := create(bobCtx, comment3, threadB1, "",
			rpcCommentPayload("No document."))
		test.IsRPCError(t, err, connect.CodeInvalidArgument)
	})

	t.Run("malformed arguments", func(t *testing.T) {
		_, err := create(bobCtx, "", threadA1, "", rpcCommentPayload("x"))
		test.IsRPCError(t, err, connect.CodeInvalidArgument)

		_, err = create(bobCtx, comment3, "not-a-uuid", "", rpcCommentPayload("x"))
		test.IsRPCError(t, err, connect.CodeInvalidArgument)

		_, err = create(bobCtx, comment3, threadA1, "", nil)
		test.IsRPCError(t, err, connect.CodeInvalidArgument)

		_, err = eu.Comments.GetThread(bobCtx, &user.GetThreadRequest{Uuid: "nope"})
		test.IsRPCError(t, err, connect.CodeInvalidArgument)
	})

	t.Run("invalid payload", func(t *testing.T) {
		payload := rpcCommentPayload("Wrong type.")
		payload.Type = "core/inbox-message"

		_, err := create(bobCtx, comment3, threadA1, "", payload)
		test.IsRPCError(t, err, connect.CodeInvalidArgument)
	})

	t.Run("inline mention without a link", func(t *testing.T) {
		payload := rpcCommentPayload(`Ask <mention id="m9">Bo</mention>.`, anna)

		_, err := create(bobCtx, comment3, threadA1, "", payload)
		test.IsRPCError(t, err, connect.CodeInvalidArgument)

		if !strings.Contains(err.Error(), `"m9"`) {
			t.Fatalf("expected the error to name the id, got %v", err)
		}
	})

	t.Run("update by the author", func(t *testing.T) {
		res, err := eu.Comments.UpdateComment(aliceCtx, &user.UpdateCommentRequest{
			Uuid:    comment1,
			Payload: rpcCommentPayload("Checked, never mind."),
		})
		test.Mustf(t, err, "update own comment")

		test.Equalf(t, 0, len(res.Comment.Mentioned),
			"mentions derived again from the new payload")

		if !parseRFC3339(t, res.Comment.Updated).After(
			parseRFC3339(t, res.Comment.Created)) {
			t.Fatalf("expected updated %s to be later than created %s",
				res.Comment.Updated, res.Comment.Created)
		}
	})

	t.Run("update by another", func(t *testing.T) {
		_, err := eu.Comments.UpdateComment(bobCtx, &user.UpdateCommentRequest{
			Uuid:    comment1,
			Payload: rpcCommentPayload("Not mine."),
		})
		test.IsRPCError(t, err, connect.CodePermissionDenied)
	})

	t.Run("update of an unknown comment", func(t *testing.T) {
		_, err := eu.Comments.UpdateComment(bobCtx, &user.UpdateCommentRequest{
			Uuid:    comment5,
			Payload: rpcCommentPayload("Nobody."),
		})
		test.IsRPCError(t, err, connect.CodeNotFound)
	})

	t.Run("delete by another without doc_admin", func(t *testing.T) {
		_, err := eu.Comments.DeleteComment(aliceCtx,
			&user.DeleteCommentRequest{Uuid: comment2})
		test.IsRPCError(t, err, connect.CodePermissionDenied)
	})

	t.Run("delete by another with doc_admin", func(t *testing.T) {
		_, err := eu.Comments.DeleteComment(adminCtx,
			&user.DeleteCommentRequest{Uuid: comment2})
		test.Mustf(t, err, "delete as admin")
	})

	t.Run("update of a deleted comment", func(t *testing.T) {
		_, err := eu.Comments.UpdateComment(bobCtx, &user.UpdateCommentRequest{
			Uuid:    comment2,
			Payload: rpcCommentPayload("Too late."),
		})
		test.IsRPCError(t, err, connect.CodeFailedPrecondition)
	})

	t.Run("delete by the author", func(t *testing.T) {
		_, err := create(bobCtx, comment3, threadA1, "",
			rpcCommentPayload("Oops."))
		test.Mustf(t, err, "create a comment to delete")

		_, err = eu.Comments.DeleteComment(bobCtx,
			&user.DeleteCommentRequest{Uuid: comment3})
		test.Mustf(t, err, "delete own comment")

		_, err = eu.Comments.DeleteComment(bobCtx,
			&user.DeleteCommentRequest{Uuid: comment5})
		test.IsRPCError(t, err, connect.CodeNotFound)
	})

	t.Run("get thread", func(t *testing.T) {
		thread := getThread(bobCtx, threadA1)

		expectStrings(t, []string{comment1, comment2, comment3},
			commentUUIDs(thread.Comments), "comments oldest first")
		test.Equalf(t, int64(1), thread.LiveComments,
			"deleted comments are not live")

		deleted := thread.Comments[1]

		test.Equalf(t, false, deleted.Deleted == "", "deleted timestamp")
		test.Equalf(t, admin, deleted.DeletedBy, "deleted by the admin")
		test.Equalf(t, true, deleted.Payload == nil,
			"no payload for a deleted comment")
		test.Equalf(t, bob, thread.Comments[2].DeletedBy,
			"deleted by the author")
		test.Equalf(t, true, thread.Comments[0].Payload != nil,
			"payload for a live comment")

		_, err := eu.Comments.GetThread(bobCtx,
			&user.GetThreadRequest{Uuid: threadB1})
		test.IsRPCError(t, err, connect.CodeNotFound)
	})

	t.Run("resolve and reopen", func(t *testing.T) {
		res, err := eu.Comments.ResolveThread(bobCtx,
			&user.ResolveThreadRequest{Uuid: threadA1})
		test.Mustf(t, err, "resolve the thread")

		test.Equalf(t, false, res.Thread.Resolved == "", "resolved timestamp")
		test.Equalf(t, bob, res.Thread.ResolvedBy, "resolved by")
		test.Equalf(t, 0, len(res.Thread.Comments),
			"resolve answers without comments")

		list, err := eu.Comments.ListThreads(bobCtx,
			&user.ListThreadsRequest{DocumentUuid: docA})
		test.Mustf(t, err, "list open threads")
		test.Equalf(t, 0, len(list.Threads), "resolved thread left out")

		list, err = eu.Comments.ListThreads(bobCtx, &user.ListThreadsRequest{
			DocumentUuid:    docA,
			IncludeResolved: true,
		})
		test.Mustf(t, err, "list all threads")
		expectStrings(t, []string{threadA1}, threadUUIDs(list.Threads),
			"resolved thread included")

		reopened, err := eu.Comments.ReopenThread(aliceCtx,
			&user.ReopenThreadRequest{Uuid: threadA1})
		test.Mustf(t, err, "reopen the thread")

		test.Equalf(t, "", reopened.Thread.Resolved, "open again")
		test.Equalf(t, "", reopened.Thread.ResolvedBy, "no resolver")
		test.Equalf(t, 0, len(reopened.Thread.Comments),
			"reopen answers without comments")

		_, err = eu.Comments.ResolveThread(bobCtx,
			&user.ResolveThreadRequest{Uuid: threadA1})
		test.Mustf(t, err, "resolve the thread again")

		posted, err := create(aliceCtx, comment4, threadA1, "",
			rpcCommentPayload("One more thing."))
		test.Mustf(t, err, "post to the resolved thread")

		test.Equalf(t, "", posted.Thread.Resolved,
			"a post reopens a resolved thread")

		_, err = eu.Comments.ResolveThread(bobCtx,
			&user.ResolveThreadRequest{Uuid: threadB1})
		test.IsRPCError(t, err, connect.CodeNotFound)
	})

	t.Run("list threads", func(t *testing.T) {
		_, err := create(bobCtx, comment5, threadA2, docA,
			rpcCommentPayload("A second thread."))
		test.Mustf(t, err, "start a second thread")

		list, err := eu.Comments.ListThreads(aliceCtx,
			&user.ListThreadsRequest{DocumentUuid: docA})
		test.Mustf(t, err, "list threads")

		expectStrings(t, []string{threadA2, threadA1}, threadUUIDs(list.Threads),
			"most recently updated first")
		expectStrings(t, []string{comment1, comment2, comment3, comment4},
			commentUUIDs(list.Threads[1].Comments),
			"each thread with its comments, oldest first")
		test.Equalf(t, int64(2), list.Threads[1].LiveComments,
			"live comments")

		_, err = eu.Comments.UpdateComment(aliceCtx, &user.UpdateCommentRequest{
			Uuid:    comment4,
			Payload: rpcCommentPayload("One more thing, edited."),
		})
		test.Mustf(t, err, "edit in the first thread")

		list, err = eu.Comments.ListThreads(aliceCtx,
			&user.ListThreadsRequest{DocumentUuid: docA})
		test.Mustf(t, err, "list threads again")

		expectStrings(t, []string{threadA1, threadA2}, threadUUIDs(list.Threads),
			"an edit moves its thread first")

		list, err = eu.Comments.ListThreads(aliceCtx,
			&user.ListThreadsRequest{DocumentUuid: docB})
		test.Mustf(t, err, "list threads on a document without any")
		test.Equalf(t, 0, len(list.Threads), "no threads")

		_, err = eu.Comments.ListThreads(aliceCtx, &user.ListThreadsRequest{})
		test.IsRPCError(t, err, connect.CodeInvalidArgument)
	})

	t.Run("document permissions", func(t *testing.T) {
		const (
			threadRefused  = "b0000000-0000-4000-8000-000000000010"
			threadReadOnly = "b0000000-0000-4000-8000-000000000011"
			docRefused     = "a0000000-0000-4000-8000-000000000010"
			docReadOnly    = "a0000000-0000-4000-8000-000000000011"
			docMissing     = "a0000000-0000-4000-8000-000000000012"
		)

		eu.Repository.Grant(docRefused, alice)
		eu.Repository.Grant(docReadOnly, alice, "r")
		eu.Repository.NotFound(docMissing)

		_, err := create(bobCtx, "c0000000-0000-4000-8000-000000000010",
			threadRefused, docRefused, rpcCommentPayload("Hidden."))
		test.Mustf(t, err, "post where bob may write")

		_, err = create(bobCtx, "c0000000-0000-4000-8000-000000000011",
			threadReadOnly, docReadOnly, rpcCommentPayload("Read only."))
		test.Mustf(t, err, "post where bob may write")

		_, err = eu.Comments.GetThread(aliceCtx,
			&user.GetThreadRequest{Uuid: threadRefused})
		test.IsRPCError(t, err, connect.CodePermissionDenied)

		_, err = eu.Comments.ListThreads(aliceCtx,
			&user.ListThreadsRequest{DocumentUuid: docRefused})
		test.IsRPCError(t, err, connect.CodePermissionDenied)

		_, err = eu.Comments.WaitForComments(aliceCtx, &user.WaitForCommentsRequest{
			DocumentUuids: []string{docReadOnly, docRefused},
			MaxWaitSec:    1,
		})
		test.IsRPCError(t, err, connect.CodePermissionDenied)

		thread := getThread(aliceCtx, threadReadOnly)
		test.Equalf(t, 1, len(thread.Comments), "read where alice may read")

		_, err = create(aliceCtx, "c0000000-0000-4000-8000-000000000012",
			threadReadOnly, "", rpcCommentPayload("Let me in."))
		test.IsRPCError(t, err, connect.CodePermissionDenied)

		_, err = create(aliceCtx, "c0000000-0000-4000-8000-000000000012",
			"b0000000-0000-4000-8000-000000000012", docReadOnly,
			rpcCommentPayload("Let me in."))
		test.IsRPCError(t, err, connect.CodePermissionDenied)

		_, err = eu.Comments.ResolveThread(aliceCtx,
			&user.ResolveThreadRequest{Uuid: threadReadOnly})
		test.IsRPCError(t, err, connect.CodePermissionDenied)

		_, err = eu.Comments.DeleteComment(adminCtx, &user.DeleteCommentRequest{
			Uuid: "c0000000-0000-4000-8000-000000000011",
		})
		test.Mustf(t, err, "delete as an admin who may write")

		_, err = create(aliceCtx, "c0000000-0000-4000-8000-000000000013",
			"b0000000-0000-4000-8000-000000000013", docMissing,
			rpcCommentPayload("Nowhere."))
		test.IsRPCError(t, err, connect.CodeNotFound)
	})

	t.Run("missing user scope", func(t *testing.T) {
		_, err := eu.Comments.GetThread(noScopeCtx,
			&user.GetThreadRequest{Uuid: threadA1})
		test.IsRPCError(t, err, connect.CodePermissionDenied)

		_, err = eu.Comments.ListMentions(noScopeCtx, &user.ListMentionsRequest{})
		test.IsRPCError(t, err, connect.CodePermissionDenied)
	})
}

// TestCommentsMentions checks that ListMentions answers the caller's own
// mentions as references, newest first, paged on the created timestamp.
func TestCommentsMentions(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	const (
		doc    = "a0000000-0000-4000-8000-000000000001"
		thread = "b0000000-0000-4000-8000-000000000001"
		anna   = "core://user/anna"
		bo     = "core://user/bo"
	)

	aliceCtx := bearerContext(ctx, eu.AccessToken(t,
		commentClaims("alice", "user", "Alice Andersson")))
	annaCtx := bearerContext(ctx, eu.AccessToken(t,
		commentClaims("anna", "user", "Anna")))
	carlCtx := bearerContext(ctx, eu.AccessToken(t,
		commentClaims("carl", "user", "")))

	var ids []string

	for i := range 5 {
		id := fmt.Sprintf("c0000000-0000-4000-8000-00000000000%d", i)
		mentions := []string{anna}

		if i == 2 {
			mentions = []string{bo}
		}

		_, err := eu.Comments.CreateComment(aliceCtx, &user.CreateCommentRequest{
			Uuid:         id,
			ThreadUuid:   thread,
			DocumentUuid: doc,
			Payload:      rpcCommentPayload(fmt.Sprintf("Comment %d", i), mentions...),
		})
		test.Mustf(t, err, "create comment %d", i)

		if i != 2 {
			ids = append(ids, id)
		}
	}

	slices.Reverse(ids)

	first, err := eu.Comments.ListMentions(annaCtx, &user.ListMentionsRequest{Size: 2})
	test.Mustf(t, err, "list the newest mentions")

	got := make([]string, 0, len(ids))

	for _, m := range first.Mentions {
		got = append(got, m.CommentUuid)

		test.Equalf(t, thread, m.ThreadUuid, "thread of the mention")
		test.Equalf(t, doc, m.DocumentUuid, "document of the mention")
		test.Equalf(t, "core://user/alice", m.Author, "author of the mention")
		test.Equalf(t, "Alice Andersson", m.AuthorName, "author name")
	}

	test.Equalf(t, 2, len(first.Mentions), "page size")

	rest, err := eu.Comments.ListMentions(annaCtx, &user.ListMentionsRequest{
		Before: first.Mentions[1].Created,
	})
	test.Mustf(t, err, "list the next page")

	for _, m := range rest.Mentions {
		got = append(got, m.CommentUuid)
	}

	expectStrings(t, ids, got, "the caller's mentions, newest first")

	none, err := eu.Comments.ListMentions(carlCtx, &user.ListMentionsRequest{})
	test.Mustf(t, err, "list mentions for someone never mentioned")
	test.Equalf(t, 0, len(none.Mentions), "only the caller's own mentions")

	_, err = eu.Comments.ListMentions(annaCtx, &user.ListMentionsRequest{Size: -1})
	test.IsRPCError(t, err, connect.CodeInvalidArgument)

	_, err = eu.Comments.ListMentions(annaCtx, &user.ListMentionsRequest{
		Before: "yesterday",
	})
	test.IsRPCError(t, err, connect.CodeInvalidArgument)
}

// TestWaitForComments covers the interim wakeup poll: it answers with the
// document that had a change, times out empty, and refuses requests over
// its limits.
func TestWaitForComments(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	const (
		docA = "a0000000-0000-4000-8000-000000000001"
		docB = "a0000000-0000-4000-8000-000000000002"
		docC = "a0000000-0000-4000-8000-000000000003"
	)

	aliceCtx := bearerContext(ctx, eu.AccessToken(t,
		commentClaims("alice", "user", "Alice Andersson")))

	t.Run("wakes on a post", func(t *testing.T) {
		type result struct {
			res *user.WaitForCommentsResponse
			err error
		}

		done := make(chan result, 1)

		go func() {
			res, err := eu.Comments.WaitForComments(aliceCtx,
				&user.WaitForCommentsRequest{
					DocumentUuids: []string{docA, docB, docA},
					MaxWaitSec:    10,
				})

			done <- result{res: res, err: err}
		}()

		// Let the wait register before the post.
		time.Sleep(50 * time.Millisecond)

		started := time.Now()

		_, err := eu.Comments.CreateComment(aliceCtx, &user.CreateCommentRequest{
			Uuid:         "c0000000-0000-4000-8000-000000000001",
			ThreadUuid:   "b0000000-0000-4000-8000-000000000001",
			DocumentUuid: docB,
			Payload:      rpcCommentPayload("Wake up."),
		})
		test.Mustf(t, err, "post to a waited-for document")

		// Let the wait answer before anything else happens.
		time.Sleep(50 * time.Millisecond)

		r := <-done
		test.Mustf(t, r.err, "wait for comments")

		expectStrings(t, []string{docB}, r.res.DocumentUuids,
			"answer with the document that changed")

		if time.Since(started) > 5*time.Second {
			t.Fatal("the wait ran to its timeout instead of waking")
		}
	})

	t.Run("times out empty", func(t *testing.T) {
		started := time.Now()

		res, err := eu.Comments.WaitForComments(aliceCtx,
			&user.WaitForCommentsRequest{
				DocumentUuids: []string{docC},
				MaxWaitSec:    1,
			})
		test.Mustf(t, err, "wait without changes")

		test.Equalf(t, 0, len(res.DocumentUuids), "nothing changed")

		if time.Since(started) < time.Second {
			t.Fatal("answered before the wait ran out")
		}
	})

	t.Run("limits", func(t *testing.T) {
		many := make([]string, 101)
		for i := range many {
			many[i] = fmt.Sprintf("a0000000-0000-4000-8000-%012d", i)
		}

		_, err := eu.Comments.WaitForComments(aliceCtx,
			&user.WaitForCommentsRequest{DocumentUuids: many})
		test.IsRPCError(t, err, connect.CodeInvalidArgument)

		_, err = eu.Comments.WaitForComments(aliceCtx,
			&user.WaitForCommentsRequest{
				DocumentUuids: []string{docA},
				MaxWaitSec:    -1,
			})
		test.IsRPCError(t, err, connect.CodeInvalidArgument)

		_, err = eu.Comments.WaitForComments(aliceCtx,
			&user.WaitForCommentsRequest{})
		test.IsRPCError(t, err, connect.CodeInvalidArgument)

		_, err = eu.Comments.WaitForComments(aliceCtx,
			&user.WaitForCommentsRequest{DocumentUuids: []string{"nope"}})
		test.IsRPCError(t, err, connect.CodeInvalidArgument)
	})
}

// TestCommentsUnauthenticated checks that a call without a token is
// unauthenticated on both stacks.
func TestCommentsUnauthenticated(t *testing.T) {
	eu := startElephantUser(t)

	for _, stack := range []rpcStack{stackTwirp, stackConnect} {
		clients := newClients(stack, eu.Client, eu.BaseURL)

		_, err := clients.Comments.GetThread(t.Context(),
			&user.GetThreadRequest{Uuid: "b0000000-0000-4000-8000-000000000001"})

		test.IsRPCError(t, err, connect.CodeUnauthenticated)
	}
}
