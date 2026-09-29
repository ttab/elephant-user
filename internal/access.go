package internal

import (
	"slices"

	"github.com/ttab/elephantine"
)

const (
	ScopeUser     = "user"
	ScopeDocAdmin = "doc_admin"
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
