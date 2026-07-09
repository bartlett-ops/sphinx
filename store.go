package main

import (
	"sort"
	"time"
)

const (
	// storeVersion is the schema tag written into every persisted document.
	storeVersion = 1
	// storeKey is the single ConfigMap data key holding the whole store.
	storeKey = "users.json"
	// maxRetries bounds every compare-and-swap loop in this package.
	maxRetries = 5
)

// Record is one user's allowlisted CIDR. UpdatedAt records when the CIDR last
// changed, not when the user was last seen; nothing in Sphinx reads it.
type Record struct {
	CIDR      string    `json:"cidr"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Store is the authoritative user document. Email is the map key, so a user has
// exactly one CIDR by construction.
type Store struct {
	Version    int               `json:"version"`
	Generation int64             `json:"generation"`
	Users      map[string]Record `json:"users"`
}

func newStore() *Store {
	return &Store{Version: storeVersion, Users: make(map[string]Record)}
}

// upsert sets email's CIDR and reports whether the store changed. Generation
// advances only on a real change, so a repeated authentication writes nothing.
func (s *Store) upsert(email, cidr string, now time.Time) bool {
	if existing, ok := s.Users[email]; ok && existing.CIDR == cidr {
		return false
	}
	s.Users[email] = Record{CIDR: cidr, UpdatedAt: now}
	s.Generation++
	return true
}

// CIDRs is the allowlist projection: every distinct CIDR, sorted for a stable
// comparison against what is already on the middleware.
func (s *Store) CIDRs() []string {
	set := make(map[string]struct{}, len(s.Users))
	for _, r := range s.Users {
		set[r.CIDR] = struct{}{}
	}
	cidrs := make([]string, 0, len(set))
	for c := range set {
		cidrs = append(cidrs, c)
	}
	sort.Strings(cidrs)
	return cidrs
}
