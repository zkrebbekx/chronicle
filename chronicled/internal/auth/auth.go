// Package auth implements chronicled's bearer-token authentication.
//
// The design decision that matters lives here: the service, not the caller,
// decides who a write is attributed to. Each configured token maps to one
// chronicle.Actor, and the handler stamps that actor on every write the token
// makes. There is no path by which a request body can name an actor — an
// audit service that accepted caller-supplied actor claims would record
// fiction with perfect formatting.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"

	"github.com/zkrebbekx/chronicle"
)

// Role is what a token is allowed to do.
type Role string

const (
	// RoleWriter may write records and corrections, and read everything.
	RoleWriter Role = "writer"
	// RoleAdmin may additionally place and release legal holds, run
	// retention sweeps, destroy subject keys, and verify hash chains.
	RoleAdmin Role = "admin"
)

// Principal is an authenticated caller: the actor its token writes as, and
// what it may do.
type Principal struct {
	Actor chronicle.Actor
	Role  Role
}

// Credential is one token table entry, already validated by config.
type Credential struct {
	Token     string
	Principal Principal
}

// Authenticator resolves presented bearer tokens against a static table.
type Authenticator struct {
	entries []entry
}

type entry struct {
	// hash is SHA-256 of the configured token. Comparing digests rather than
	// the tokens themselves gives every comparison the same length whatever
	// the presented token's length, so the constant-time compare below is
	// constant-time in practice and not just in name.
	hash      [sha256.Size]byte
	principal Principal
}

// New builds an authenticator from the credential table. Config has already
// validated the table; the checks here are the cheap invariants this package
// itself depends on.
func New(creds []Credential) (*Authenticator, error) {
	if len(creds) == 0 {
		return nil, fmt.Errorf("auth: no credentials configured")
	}
	a := &Authenticator{entries: make([]entry, 0, len(creds))}
	for _, c := range creds {
		if c.Token == "" {
			return nil, fmt.Errorf("auth: empty token")
		}
		if c.Principal.Actor.ID == "" {
			return nil, fmt.Errorf("auth: token has no actor ID")
		}
		if c.Principal.Role != RoleWriter && c.Principal.Role != RoleAdmin {
			return nil, fmt.Errorf("auth: unknown role %q", c.Principal.Role)
		}
		a.entries = append(a.entries, entry{
			hash:      sha256.Sum256([]byte(c.Token)),
			principal: c.Principal,
		})
	}
	return a, nil
}

// Authenticate resolves a presented token. The comparison against every
// configured token uses crypto/subtle's constant-time compare over fixed-size
// digests, and the loop never exits early, so response timing does not narrow
// down a token byte by byte. The token table is small and static; scanning
// all of it on every request is the price of that property and it is cheap.
func (a *Authenticator) Authenticate(token string) (Principal, bool) {
	h := sha256.Sum256([]byte(token))
	var (
		found Principal
		ok    bool
	)
	for i := range a.entries {
		if subtle.ConstantTimeCompare(h[:], a.entries[i].hash[:]) == 1 {
			found = a.entries[i].principal
			ok = true
		}
	}
	return found, ok
}
