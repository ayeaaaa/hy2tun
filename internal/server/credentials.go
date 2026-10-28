package server

import (
	"net"

	hyserver "github.com/apernet/hysteria/core/v2/server"

	"hy2tun/internal/config"
	"hy2tun/internal/credentials"
)

// dynamicAuthenticator validates HY2 passwords against the live credential
// store, so a rotation applies to new connections without restarting the
// server. The optional fallback is consulted when the store has no match.
type dynamicAuthenticator struct {
	store    *credentials.Store
	fallback hyserver.Authenticator
}

// newDynamicAuthenticator returns an authenticator backed by store.
func newDynamicAuthenticator(store *credentials.Store, fallback hyserver.Authenticator) hyserver.Authenticator {
	return &dynamicAuthenticator{store: store, fallback: fallback}
}

// Authenticate accepts the current or previous HY2 password. Both map to the
// single "user" identity because password authentication has one account.
func (a *dynamicAuthenticator) Authenticate(addr net.Addr, auth string, tx uint64) (bool, string) {
	if a != nil && a.store != nil {
		if set := a.store.Load(); set.AcceptAuth(auth) {
			return true, "user"
		}
	}
	if a != nil && a.fallback != nil {
		return a.fallback.Authenticate(addr, auth, tx)
	}
	return false, ""
}

// credentialSetFromConfig builds the initial runtime credential set. The
// current control token lives in control.token; the previous secrets are
// persisted under credentials.* so they keep working across restarts.
func credentialSetFromConfig(cfg *config.ServerConfig) *credentials.Set {
	if cfg == nil {
		return nil
	}
	generation := cfg.Credentials.Generation
	if generation < 1 {
		generation = 1
	}
	return &credentials.Set{
		Generation:           generation,
		Auth:                 cfg.Auth.Password,
		ControlToken:         cfg.Control.Token,
		PreviousGeneration:   cfg.Credentials.PreviousGeneration,
		PreviousAuth:         cfg.Credentials.PreviousAuth,
		PreviousControlToken: cfg.Credentials.PreviousControlToken,
	}
}

// currentCredentials returns the active credential set. The returned value is
// an independent copy and may be read without holding a lock.
func (s *Server) currentCredentials() *credentials.Set {
	if s == nil {
		return nil
	}
	return s.credentials.Load()
}
