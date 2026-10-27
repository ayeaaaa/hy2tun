package ports

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPolicyAllowDenyPrecedence(t *testing.T) {
	p, err := NewPolicy([]string{"2222", "8000-8002"}, []string{"8001"}, "20000-20002")
	require.NoError(t, err)
	require.NoError(t, p.Check(2222))
	require.NoError(t, p.Check(8000))
	require.ErrorIs(t, p.Check(8001), ErrDenied)
	require.ErrorIs(t, p.Check(9000), ErrNotAllowed)
}

func TestAllocateSkipsBusyAndSoftReserved(t *testing.T) {
	p, err := NewPolicy(nil, nil, "20000-20002")
	require.NoError(t, err)
	// isFree returns true when a port is free, so 20000 is busy here.
	got, err := p.Allocate(0,
		func(port int) bool { return port != 20000 },
		func(port int) bool { return port == 20001 })
	require.NoError(t, err)
	require.Equal(t, 20002, got)
}

func TestAllocateExhausted(t *testing.T) {
	p, _ := NewPolicy(nil, nil, "20000-20000")
	// No port is free.
	_, err := p.Allocate(0, func(int) bool { return false }, func(int) bool { return false })
	require.ErrorIs(t, err, ErrNoFreePort)
}

func TestParseRangesRejectsReversedAndOutOfBounds(t *testing.T) {
	_, err := ParseRanges([]string{"9000-8000"})
	require.Error(t, err)
	_, err = ParseRanges([]string{"70000"})
	require.Error(t, err)
}

func TestParseRangesAcceptsSinglesAndInclusiveRanges(t *testing.T) {
	got, err := ParseRanges([]string{"2222", "8000-8002", " 9000 - 9001 "})
	require.NoError(t, err)
	require.Equal(t, []Range{
		{Start: 2222, End: 2222},
		{Start: 8000, End: 8002},
		{Start: 9000, End: 9001},
	}, got)
}

func TestParseRangesRejectsInvalidSpecs(t *testing.T) {
	for _, spec := range []string{"", "abc", "0", "65536", "-1", "8000-", "-8000", "8000-8100-8200"} {
		_, err := ParseRanges([]string{spec})
		require.Error(t, err, spec)
	}
}

func TestAllocateExplicitRequest(t *testing.T) {
	p, err := NewPolicy([]string{"2222"}, []string{"8001"}, "20000-20002")
	require.NoError(t, err)

	// Soft reservations never block an explicit request.
	got, err := p.Allocate(2222, func(int) bool { return true }, func(int) bool { return true })
	require.NoError(t, err)
	require.Equal(t, 2222, got)

	_, err = p.Allocate(2222, func(int) bool { return false }, func(int) bool { return false })
	require.ErrorIs(t, err, ErrNoFreePort)

	_, err = p.Allocate(8001, func(int) bool { return true }, func(int) bool { return false })
	require.ErrorIs(t, err, ErrDenied)

	_, err = p.Allocate(9000, func(int) bool { return true }, func(int) bool { return false })
	require.ErrorIs(t, err, ErrNotAllowed)
}

func TestAllocateAutoUsesAllowRangesWhenPresent(t *testing.T) {
	p, err := NewPolicy([]string{"9000-9001", "8000"}, nil, "20000-20002")
	require.NoError(t, err)
	got, err := p.Allocate(0, func(int) bool { return true }, func(int) bool { return false })
	require.NoError(t, err)
	require.Equal(t, 8000, got, "auto allocation must scan allow ranges in ascending order")
}

func TestAllocateAutoSkipsDeniedPorts(t *testing.T) {
	p, err := NewPolicy(nil, []string{"20000"}, "20000-20001")
	require.NoError(t, err)
	got, err := p.Allocate(0, func(int) bool { return true }, func(int) bool { return false })
	require.NoError(t, err)
	require.Equal(t, 20001, got)
}

func TestAllocateNoCandidates(t *testing.T) {
	p, err := NewPolicy(nil, nil, "")
	require.NoError(t, err)
	_, err = p.Allocate(0, func(int) bool { return true }, func(int) bool { return false })
	require.ErrorIs(t, err, ErrNoFreePort)
}
