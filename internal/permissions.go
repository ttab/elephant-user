package internal

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/jellydator/ttlcache/v3"
	"github.com/ttab/elephant-api/repository"
	"github.com/ttab/elephant-api/repository/repositoryconnect"
	"github.com/ttab/elephantine/rpc"
)

// Permission keys in the repository's GetPermissions response.
const (
	permissionRead      = "r"
	permissionWrite     = "w"
	permissionMetaWrite = "m"
)

// DefaultPermissionsCacheTTL is how long a permission answer from the
// repository is reused for the same subject and document.
const DefaultPermissionsCacheTTL = 30 * time.Second

// ErrDocumentNotFound is returned by DocumentPermissions when the repository
// answers not_found for the document.
var ErrDocumentNotFound = errors.New("document not found")

// DocumentPermissions answers what the calling user may do with a repository
// document. The caller is read from the request context.
type DocumentPermissions interface {
	// Permissions returns the caller's permissions on the document.
	// Nothing granted is not an error: a caller the repository refuses
	// outright gets an empty set, so a handler only has to check the flag
	// it needs and answer permission_denied when it is false. The error is
	// ErrDocumentNotFound for an unknown document, and anything else is a
	// failed lookup that the handler surfaces as internal.
	Permissions(
		ctx context.Context, documentUUID string,
	) (DocumentPermissionSet, error)
}

// DocumentPermissionSet is the caller's permissions on one document.
type DocumentPermissionSet struct {
	Read      bool
	Write     bool
	MetaWrite bool
	// Grants is the repository's answer as is: the permission key mapped
	// to what granted it, the caller's subject, a unit URI, or
	// "scope://<name>" for an elevated grant.
	Grants map[string]string
}

func permissionSetFromGrants(grants map[string]string) DocumentPermissionSet {
	_, read := grants[permissionRead]
	_, write := grants[permissionWrite]
	_, metaWrite := grants[permissionMetaWrite]

	return DocumentPermissionSet{
		Read:      read,
		Write:     write,
		MetaWrite: metaWrite,
		Grants:    grants,
	}
}

// NoDocumentPermissions grants nothing on any document. It stands in for the
// repository lookup when no repository endpoint is configured, so that the
// handlers that depend on DocumentPermissions answer permission_denied.
type NoDocumentPermissions struct{}

// Permissions implements DocumentPermissions.
func (NoDocumentPermissions) Permissions(
	_ context.Context, _ string,
) (DocumentPermissionSet, error) {
	return DocumentPermissionSet{}, nil
}

// RepositoryDocumentPermissionsOption configures a
// RepositoryDocumentPermissions.
type RepositoryDocumentPermissionsOption func(p *RepositoryDocumentPermissions)

// WithPermissionsCacheTTL sets how long an answer is cached, overriding
// DefaultPermissionsCacheTTL.
func WithPermissionsCacheTTL(ttl time.Duration) RepositoryDocumentPermissionsOption {
	return func(p *RepositoryDocumentPermissions) {
		p.ttl = ttl
	}
}

type permissionsCacheKey struct {
	Subject      string
	DocumentUUID string
}

// RepositoryDocumentPermissions asks elephant-repository's
// Documents.GetPermissions for the caller's permissions, calling it with the
// caller's own bearer token, and caches the answer per subject and document.
//
// Negative answers are cached as well as positive ones: a caller who may not
// read a document keeps being refused for the cache TTL after an ACL change
// that would let them in, as a caller who lost access keeps it for as long.
// The TTL is not extended by hits. A repository that refuses the token
// itself (permission_denied, a token without any document scope) is cached
// as an empty set. Failed lookups and not_found are not cached.
type RepositoryDocumentPermissions struct {
	client  repository.Documents
	metrics *Metrics
	ttl     time.Duration
	cache   *ttlcache.Cache[permissionsCacheKey, DocumentPermissionSet]
}

// NewRepositoryDocumentPermissions creates a permissions lookup against the
// repository at endpoint, using the Connect protocol over httpClient. The
// cache's expiry sweep runs until ctx is cancelled.
func NewRepositoryDocumentPermissions(
	ctx context.Context,
	httpClient connect.HTTPClient, endpoint string,
	metrics *Metrics,
	opts ...RepositoryDocumentPermissionsOption,
) *RepositoryDocumentPermissions {
	p := RepositoryDocumentPermissions{
		client: repositoryconnect.NewDocumentsServiceClient(
			httpClient, endpoint,
			connect.WithInterceptors(rpc.PropagateHeaders())),
		metrics: metrics,
		ttl:     DefaultPermissionsCacheTTL,
	}

	for _, opt := range opts {
		opt(&p)
	}

	p.cache = ttlcache.New(
		ttlcache.WithTTL[permissionsCacheKey, DocumentPermissionSet](p.ttl),
		ttlcache.WithDisableTouchOnHit[permissionsCacheKey, DocumentPermissionSet](),
	)

	go p.cache.Start()

	go func() {
		<-ctx.Done()
		p.cache.Stop()
	}()

	return &p
}

// Interface guard.
var _ DocumentPermissions = &RepositoryDocumentPermissions{}

// Permissions implements DocumentPermissions.
func (p *RepositoryDocumentPermissions) Permissions(
	ctx context.Context, documentUUID string,
) (DocumentPermissionSet, error) {
	auth, ok := rpc.GetAuthInfo(ctx)
	if !ok {
		return DocumentPermissionSet{}, errors.New(
			"no authenticated caller to look up permissions for")
	}

	key := permissionsCacheKey{
		Subject:      auth.Claims.Subject,
		DocumentUUID: documentUUID,
	}

	item := p.cache.Get(key)
	if item != nil {
		p.countLookup(PermissionLookupHit)

		return item.Value(), nil
	}

	// The authentication middleware only accepts bearer tokens and keeps
	// the token without its scheme.
	ctx = rpc.WithOutgoingHeaders(ctx, http.Header{
		"Authorization": []string{"Bearer " + auth.Token},
	})

	res, err := p.client.GetPermissions(ctx, &repository.GetPermissionsRequest{
		Uuid: documentUUID,
	})
	if err != nil {
		return p.failedLookup(key, err)
	}

	p.countLookup(PermissionLookupMiss)

	set := permissionSetFromGrants(res.Permissions)

	p.cache.Set(key, set, ttlcache.DefaultTTL)

	return set, nil
}

// failedLookup maps an error from the repository to the answer the handler
// gets.
func (p *RepositoryDocumentPermissions) failedLookup(
	key permissionsCacheKey, err error,
) (DocumentPermissionSet, error) {
	switch connect.CodeOf(err) { //nolint:exhaustive // Every other code is a failed lookup.
	case connect.CodePermissionDenied:
		p.countLookup(PermissionLookupDenied)

		var set DocumentPermissionSet

		p.cache.Set(key, set, ttlcache.DefaultTTL)

		return set, nil
	case connect.CodeNotFound:
		p.countLookup(PermissionLookupNotFound)

		return DocumentPermissionSet{}, ErrDocumentNotFound
	default:
		p.countLookup(PermissionLookupError)

		return DocumentPermissionSet{}, fmt.Errorf(
			"get document permissions from the repository: %w", err)
	}
}

func (p *RepositoryDocumentPermissions) countLookup(outcome string) {
	if p.metrics == nil {
		return
	}

	p.metrics.PermissionLookups.WithLabelValues(outcome).Inc()
}
