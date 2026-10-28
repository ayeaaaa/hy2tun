// Package credentials holds the runtime credential pair used by the server
// and client. A set carries the current HY2 password and control token plus
// the previous pair retained during rotation, so old credentials keep working
// until they are explicitly revoked.
package credentials

import (
	"crypto/subtle"
	"sync/atomic"
)

// Set is one generation of credentials. The previous pair is only active
// while PreviousGeneration is positive.
type Set struct {
	Generation           int64  `json:"generation"`
	Auth                 string `json:"auth"`
	ControlToken         string `json:"control_token"`
	PreviousGeneration   int64  `json:"previous_generation"`
	PreviousAuth         string `json:"previous_auth"`
	PreviousControlToken string `json:"previous_control_token"`
}

// Clone returns a shallow copy of s. A nil set clones to nil.
func (s *Set) Clone() *Set {
	if s == nil {
		return nil
	}
	clone := *s
	return &clone
}

// AcceptAuth reports whether auth matches the current or previous password.
// Empty credentials are never accepted.
func (s *Set) AcceptAuth(auth string) bool {
	if s == nil || auth == "" {
		return false
	}
	if s.Auth != "" && subtle.ConstantTimeCompare([]byte(auth), []byte(s.Auth)) == 1 {
		return true
	}
	if s.PreviousGeneration > 0 && s.PreviousAuth != "" &&
		subtle.ConstantTimeCompare([]byte(auth), []byte(s.PreviousAuth)) == 1 {
		return true
	}
	return false
}

// AcceptControlToken reports whether token matches the current or previous
// control token and returns the generation that matched. The current token is
// checked first. Empty tokens are never accepted.
func (s *Set) AcceptControlToken(token string) (generation int64, ok bool) {
	if s == nil || token == "" {
		return 0, false
	}
	if s.ControlToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.ControlToken)) == 1 {
		return s.Generation, true
	}
	if s.PreviousGeneration > 0 && s.PreviousControlToken != "" &&
		subtle.ConstantTimeCompare([]byte(token), []byte(s.PreviousControlToken)) == 1 {
		return s.PreviousGeneration, true
	}
	return 0, false
}

// Synced reports whether generation is the current generation.
func (s *Set) Synced(generation int64) bool {
	return s != nil && generation == s.Generation
}

// Store holds the active Set in an atomic pointer so readers never observe a
// partially updated credential pair.
type Store struct {
	current atomic.Pointer[Set]
}

// NewStore returns a store initialized with set.
func NewStore(set *Set) *Store {
	store := &Store{}
	store.Store(set)
	return store
}

// Load returns an independent copy of the current set. Callers may read it
// freely but must not rely on mutations becoming visible to the store.
func (s *Store) Load() *Set {
	if s == nil {
		return nil
	}
	return s.current.Load().Clone()
}

// Store atomically replaces the current set with a copy of set.
func (s *Store) Store(set *Set) {
	if s == nil {
		return
	}
	s.current.Store(set.Clone())
}
