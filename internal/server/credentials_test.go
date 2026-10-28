package server

import (
	"testing"

	"github.com/apernet/hysteria/extras/v2/auth"
	"github.com/stretchr/testify/require"

	"hy2tun/internal/config"
	"hy2tun/internal/credentials"
)

func TestDynamicAuthenticatorAcceptsCurrentAndPrevious(t *testing.T) {
	store := credentials.NewStore(&credentials.Set{
		Generation: 2, Auth: "new",
		PreviousGeneration: 1, PreviousAuth: "old",
	})
	a := newDynamicAuthenticator(store, nil)
	ok, id := a.Authenticate(nil, "new", 0)
	require.True(t, ok)
	require.Equal(t, "user", id)
	ok, _ = a.Authenticate(nil, "old", 0)
	require.True(t, ok)
	ok, _ = a.Authenticate(nil, "other", 0)
	require.False(t, ok)
}

func TestDynamicAuthenticatorFallsBackAndRejectsEmpty(t *testing.T) {
	store := credentials.NewStore(&credentials.Set{Generation: 1, Auth: "current"})
	a := newDynamicAuthenticator(store, &auth.PasswordAuthenticator{Password: "fallback"})

	ok, id := a.Authenticate(nil, "fallback", 0)
	require.True(t, ok)
	require.Equal(t, "user", id)
	ok, _ = a.Authenticate(nil, "", 0)
	require.False(t, ok)
}

func TestDynamicAuthenticatorWithoutStoreRejects(t *testing.T) {
	a := newDynamicAuthenticator(nil, nil)
	ok, _ := a.Authenticate(nil, "anything", 0)
	require.False(t, ok)
}

func TestCredentialSetFromConfigIncludesCurrentAndPrevious(t *testing.T) {
	cfg := &config.ServerConfig{
		Auth:    config.AuthConfig{Type: "password", Password: "current-auth"},
		Control: config.ControlConfig{Token: "current-token"},
		Credentials: config.CredentialsConfig{
			Generation:           2,
			PreviousGeneration:   1,
			PreviousAuth:         "old-auth",
			PreviousControlToken: "old-token",
		},
	}
	set := credentialSetFromConfig(cfg)
	require.Equal(t, int64(2), set.Generation)
	require.Equal(t, "current-auth", set.Auth)
	require.Equal(t, "current-token", set.ControlToken)
	require.True(t, set.AcceptAuth("old-auth"))
	generation, ok := set.AcceptControlToken("old-token")
	require.True(t, ok)
	require.Equal(t, int64(1), generation)

	cfg.Credentials.Generation = 0
	set = credentialSetFromConfig(cfg)
	require.Equal(t, int64(1), set.Generation)
}
