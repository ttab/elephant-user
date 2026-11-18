package internal

import (
	"slices"
	"strings"

	"github.com/ttab/elephantine"
)

const (
	ScopeUser     = "user"
	ScopeDocAdmin = "doc_admin"
	// ScopeUserNotify lets a producer ingest notification feed events.
	// It is the only scope that may.
	ScopeUserNotify = "user_notify"
)

func getAllOwners(auth *elephantine.AuthInfo) []string {
	owners := []string{auth.Claims.Subject}

	if auth.Claims.Org != "" {
		owners = append(owners, auth.Claims.Org)
	}

	owners = append(owners, auth.Claims.Units...)

	return owners
}

func isAllowedOwner(auth *elephantine.AuthInfo, owner string) bool {
	if owner == auth.Claims.Subject {
		return true
	}

	if owner == auth.Claims.Org {
		return true
	}

	return slices.Contains(auth.Claims.Units, owner)
}

// Prefixes of the uris a feed key can have. A user is any subject a token
// can carry, the same three forms the comments schema accepts for a
// mention: a core user, an application on client credentials, or a
// keycloak user. elephantine passes a sub claim with a scheme through as
// is, so the keycloak form reaches the handlers unchanged.
const (
	coreUserURIPrefix        = "core://user/"
	coreApplicationURIPrefix = "core://application/"
	keycloakUserURIPrefix    = "keycloak://user/"
	unitURIPrefix            = "core://unit/"
	orgURIPrefix             = "core://org/"
)

// hasURIPrefix reports whether value is prefix followed by a non-empty id.
func hasURIPrefix(value string, prefix string) bool {
	return strings.HasPrefix(value, prefix) && len(value) > len(prefix)
}

// isUserURI reports whether value is a user uri in one of the three forms
// a token's subject can take, followed by an id. They are what
// elephantine's claimsToSubject can produce for a sub claim: a sub that is
// already a uri kept as is (keycloak://user/), an application token's
// client id (core://application/), or a bare sub under core://user/.
func isUserURI(value string) bool {
	return hasURIPrefix(value, coreUserURIPrefix) ||
		hasURIPrefix(value, coreApplicationURIPrefix) ||
		hasURIPrefix(value, keycloakUserURIPrefix)
}

// isFeedKey reports whether a key is a user uri in any of its three forms,
// a core://unit/ uri or a core://org/ uri.
func isFeedKey(key string) bool {
	return isUserURI(key) ||
		hasURIPrefix(key, unitURIPrefix) ||
		hasURIPrefix(key, orgURIPrefix)
}
