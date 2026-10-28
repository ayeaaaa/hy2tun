package credentials

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAcceptCurrentAndPrevious(t *testing.T) {
	s := &Set{
		Generation: 2, Auth: "new", ControlToken: "new-token",
		PreviousGeneration: 1, PreviousAuth: "old", PreviousControlToken: "old-token",
	}
	require.True(t, s.AcceptAuth("new"))
	require.True(t, s.AcceptAuth("old"))
	require.False(t, s.AcceptAuth("other"))
	gen, ok := s.AcceptControlToken("new-token")
	require.True(t, ok)
	require.Equal(t, int64(2), gen)
	gen, ok = s.AcceptControlToken("old-token")
	require.True(t, ok)
	require.Equal(t, int64(1), gen)
	require.True(t, s.Synced(2))
	require.False(t, s.Synced(1))
}

func TestAcceptRejectsEmptyAndInactivePrevious(t *testing.T) {
	s := &Set{Generation: 2, Auth: "new", ControlToken: "new-token"}
	require.False(t, s.AcceptAuth(""))
	require.False(t, s.AcceptAuth("old"))

	// Previous secrets without a previous generation are not active.
	s.PreviousAuth = "old"
	s.PreviousControlToken = "old-token"
	require.False(t, s.AcceptAuth("old"))
	_, ok := s.AcceptControlToken("old-token")
	require.False(t, ok)

	_, ok = s.AcceptControlToken("")
	require.False(t, ok)
}

func TestStoreReturnsIndependentCopies(t *testing.T) {
	store := NewStore(&Set{Generation: 1, Auth: "a", ControlToken: "t"})

	first := store.Load()
	require.Equal(t, int64(1), first.Generation)
	first.Auth = "mutated"
	require.Equal(t, "a", store.Load().Auth)

	next := &Set{Generation: 2, Auth: "b", ControlToken: "u"}
	store.Store(next)
	next.Auth = "mutated"
	require.Equal(t, "b", store.Load().Auth)
	require.Equal(t, int64(2), store.Load().Generation)
}

func TestStoreNilSet(t *testing.T) {
	store := NewStore(nil)
	require.Nil(t, store.Load())

	store.Store(&Set{Generation: 1, Auth: "a", ControlToken: "t"})
	require.Equal(t, int64(1), store.Load().Generation)
}
