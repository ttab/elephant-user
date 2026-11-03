package internal_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ttab/elephant-api/repository"
	"github.com/ttab/elephant-api/repository/repositoryconnect"
	"github.com/ttab/elephant-user/internal"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/test"
)

// permissionsStub is a repository Documents service that answers
// GetPermissions with whatever the test has set for the document.
type permissionsStub struct {
	repositoryconnect.UnimplementedDocumentsHandler

	mu            sync.Mutex
	calls         int
	authorization []string
	grants        map[string]map[string]string
	errs          map[string]error
}

func (s *permissionsStub) GetPermissions(
	_ context.Context, req *connect.Request[repository.GetPermissionsRequest],
) (*connect.Response[repository.GetPermissionsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls++
	s.authorization = append(s.authorization,
		req.Header().Get("Authorization"))

	err := s.errs[req.Msg.Uuid]
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&repository.GetPermissionsResponse{
		Permissions: s.grants[req.Msg.Uuid],
	}), nil
}

func (s *permissionsStub) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls
}

func (s *permissionsStub) lastAuthorization() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.authorization[len(s.authorization)-1]
}

func TestRepositoryDocumentPermissions(t *testing.T) {
	const (
		writableDoc = "1a0dc8b4-0d5d-4c34-9a6b-fb4ba0e5c4f1"
		readableDoc = "2b1ed9c5-1e6e-4d45-8b7c-0c5cb1f6d502"
		refusedDoc  = "3c2fead6-2f7f-4e56-9c8d-1d6dc207e613"
		missingDoc  = "4d30fbe7-3080-4f67-8d9e-2e7ed318f724"
		brokenDoc   = "5e410cf8-4191-4078-9eaf-3f8fe429a835"
		subject     = "core://user/tester"
		token       = "the-callers-token"
	)

	stub := &permissionsStub{
		grants: map[string]map[string]string{
			writableDoc: {
				"r": subject,
				"w": "core://unit/desk",
			},
			readableDoc: {
				"r": "scope://doc_read_all",
			},
		},
		errs: map[string]error{
			refusedDoc: connect.NewError(connect.CodePermissionDenied,
				errors.New("no document scope")),
			missingDoc: connect.NewError(connect.CodeNotFound,
				errors.New("no such document")),
			brokenDoc: connect.NewError(connect.CodeInternal,
				errors.New("database down")),
		},
	}

	path, handler := repositoryconnect.NewDocumentsHandler(stub)

	mux := http.NewServeMux()
	mux.Handle(path, handler)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	metrics, err := internal.NewMetrics(prometheus.NewRegistry())
	test.Mustf(t, err, "create metrics")

	perms := internal.NewRepositoryDocumentPermissions(
		t.Context(), server.Client(), server.URL, metrics)

	ctx := elephantine.SetAuthInfo(t.Context(), &elephantine.AuthInfo{
		Token: token,
		Claims: elephantine.JWTClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				Subject: subject,
			},
			Scope: "user doc_read",
		},
	})

	lookups := func(outcome string) float64 {
		return testutil.ToFloat64(
			metrics.PermissionLookups.WithLabelValues(outcome))
	}

	set, err := perms.Permissions(ctx, writableDoc)
	test.Mustf(t, err, "look up a writable document")

	test.Equalf(t, "Bearer "+token, stub.lastAuthorization(),
		"forward the caller's authorization")
	test.Equalf(t, true, set.Read, "read granted")
	test.Equalf(t, true, set.Write, "write granted")
	test.Equalf(t, false, set.MetaWrite, "meta write not granted")
	test.Equalf(t, "core://unit/desk", set.Grants["w"],
		"keep what granted write")

	_, err = perms.Permissions(ctx, writableDoc)
	test.Mustf(t, err, "look up the writable document again")

	test.Equalf(t, 1, stub.callCount(),
		"answer the second lookup from the cache")
	test.Equalf(t, 1.0, lookups(internal.PermissionLookupMiss), "misses")
	test.Equalf(t, 1.0, lookups(internal.PermissionLookupHit), "hits")

	set, err = perms.Permissions(ctx, readableDoc)
	test.Mustf(t, err, "look up a readable document")

	test.Equalf(t, true, set.Read, "read granted by scope")
	test.Equalf(t, false, set.Write, "write not granted")

	set, err = perms.Permissions(ctx, refusedDoc)
	test.Mustf(t, err, "look up with a refused token")

	test.Equalf(t, false, set.Read, "read not granted")
	test.Equalf(t, false, set.Write, "write not granted")
	test.Equalf(t, 1.0, lookups(internal.PermissionLookupDenied), "denials")

	callsBefore := stub.callCount()

	_, err = perms.Permissions(ctx, refusedDoc)
	test.Mustf(t, err, "look up with a refused token again")

	test.Equalf(t, callsBefore, stub.callCount(), "cache the refusal")

	_, err = perms.Permissions(ctx, missingDoc)
	test.Equalf(t, true, errors.Is(err, internal.ErrDocumentNotFound),
		"report an unknown document as not found")

	_, err = perms.Permissions(ctx, brokenDoc)
	if err == nil || errors.Is(err, internal.ErrDocumentNotFound) {
		t.Fatalf("expected a failed lookup for an upstream internal error, got %v", err)
	}

	test.Equalf(t, 1.0, lookups(internal.PermissionLookupError), "errors")

	callsBefore = stub.callCount()

	_, err = perms.Permissions(ctx, brokenDoc)
	test.Equalf(t, true, err != nil, "fail the lookup again")
	test.Equalf(t, callsBefore+1, stub.callCount(),
		"do not cache a failed lookup")
}

func TestNoDocumentPermissions(t *testing.T) {
	set, err := internal.NoDocumentPermissions{}.Permissions(
		t.Context(), "1a0dc8b4-0d5d-4c34-9a6b-fb4ba0e5c4f1")
	test.Mustf(t, err, "look up permissions")

	test.Equalf(t, false, set.Read, "read not granted")
	test.Equalf(t, false, set.Write, "write not granted")
}
