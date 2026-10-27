// Package ports implements public-port range parsing and the server-side
// allow/deny/auto allocation policy.
package ports

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	minPort = 1
	maxPort = 65535
)

var (
	// ErrNotAllowed reports a port outside the allow list.
	ErrNotAllowed = errors.New("port is not allowed")
	// ErrDenied reports a port matched by the deny list.
	ErrDenied = errors.New("port is denied")
	// ErrNoFreePort reports that no candidate port could be allocated.
	ErrNoFreePort = errors.New("no free port available")
)

// Range is an inclusive range of ports.
type Range struct {
	Start int
	End   int
}

func (r Range) contains(port int) bool {
	return port >= r.Start && port <= r.End
}

// ParseRanges parses port specifications such as "2222" and "8000-8100".
// Ranges are inclusive. Empty, non-numeric, out-of-bounds, and reversed
// entries are rejected.
func ParseRanges(items []string) ([]Range, error) {
	ranges := make([]Range, 0, len(items))
	for _, item := range items {
		r, err := parseRange(item)
		if err != nil {
			return nil, err
		}
		ranges = append(ranges, r)
	}
	return ranges, nil
}

func parseRange(spec string) (Range, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Range{}, fmt.Errorf("port range must not be empty")
	}
	startText, endText, hasEnd := strings.Cut(spec, "-")
	if !hasEnd {
		port, err := parsePort(startText)
		if err != nil {
			return Range{}, err
		}
		return Range{Start: port, End: port}, nil
	}
	if strings.Contains(endText, "-") {
		return Range{}, fmt.Errorf("invalid port range %q", spec)
	}
	start, err := parsePort(startText)
	if err != nil {
		return Range{}, err
	}
	end, err := parsePort(endText)
	if err != nil {
		return Range{}, err
	}
	if start > end {
		return Range{}, fmt.Errorf("invalid port range %q: start %d is after end %d", spec, start, end)
	}
	return Range{Start: start, End: end}, nil
}

func parsePort(text string) (int, error) {
	text = strings.TrimSpace(text)
	port, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("invalid port %q: must be an integer", text)
	}
	if port < minPort || port > maxPort {
		return 0, fmt.Errorf("invalid port %d: must be between %d and %d", port, minPort, maxPort)
	}
	return port, nil
}

// Policy decides which public ports may be used and allocates free ports.
// Deny entries always win over allow entries; an empty allow list permits
// every port that is not denied.
type Policy struct {
	allow []Range
	deny  []Range
	auto  []Range
}

// NewPolicy parses the allow, deny, and auto-allocation range specifications.
// An empty auto specification disables automatic allocation unless the allow
// list is non-empty, in which case the allow ranges are used.
func NewPolicy(allow, deny []string, auto string) (*Policy, error) {
	allowRanges, err := ParseRanges(allow)
	if err != nil {
		return nil, fmt.Errorf("parse allow ports: %w", err)
	}
	denyRanges, err := ParseRanges(deny)
	if err != nil {
		return nil, fmt.Errorf("parse deny ports: %w", err)
	}
	var autoRanges []Range
	if strings.TrimSpace(auto) != "" {
		autoRanges, err = ParseRanges([]string{auto})
		if err != nil {
			return nil, fmt.Errorf("parse auto ports: %w", err)
		}
	}
	return &Policy{
		allow: normalizeRanges(allowRanges),
		deny:  normalizeRanges(denyRanges),
		auto:  normalizeRanges(autoRanges),
	}, nil
}

// Check reports whether port may be used. Deny entries take precedence over
// allow entries.
func (p *Policy) Check(port int) error {
	if port < minPort || port > maxPort {
		return fmt.Errorf("%w: port %d is outside %d-%d", ErrNotAllowed, port, minPort, maxPort)
	}
	if containsPort(p.deny, port) {
		return fmt.Errorf("%w: port %d", ErrDenied, port)
	}
	if len(p.allow) > 0 && !containsPort(p.allow, port) {
		return fmt.Errorf("%w: port %d", ErrNotAllowed, port)
	}
	return nil
}

// Allocate returns a usable port. An explicit requested port is checked
// against the policy and returned when free; soft reservations never block
// explicit requests. With requested == 0, the auto range is scanned in
// ascending order, falling back to the allow ranges when they are non-empty.
func (p *Policy) Allocate(requested int, isFree func(int) bool, isSoftReserved func(int) bool) (int, error) {
	if isFree == nil {
		isFree = func(int) bool { return true }
	}
	if isSoftReserved == nil {
		isSoftReserved = func(int) bool { return false }
	}
	if requested > 0 {
		if err := p.Check(requested); err != nil {
			return 0, err
		}
		if !isFree(requested) {
			return 0, fmt.Errorf("%w: port %d is in use", ErrNoFreePort, requested)
		}
		return requested, nil
	}

	candidates := p.auto
	if len(p.allow) > 0 {
		candidates = p.allow
	}
	for _, r := range candidates {
		for port := r.Start; port <= r.End; port++ {
			if p.Check(port) != nil {
				continue
			}
			if !isFree(port) || isSoftReserved(port) {
				continue
			}
			return port, nil
		}
	}
	return 0, ErrNoFreePort
}

func containsPort(ranges []Range, port int) bool {
	for _, r := range ranges {
		if r.contains(port) {
			return true
		}
	}
	return false
}

// normalizeRanges sorts ranges by start port and merges overlapping or
// adjacent ranges so allocation visits ports in ascending order.
func normalizeRanges(ranges []Range) []Range {
	if len(ranges) < 2 {
		return ranges
	}
	sorted := append([]Range(nil), ranges...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Start != sorted[j].Start {
			return sorted[i].Start < sorted[j].Start
		}
		return sorted[i].End < sorted[j].End
	})
	merged := make([]Range, 0, len(sorted))
	merged = append(merged, sorted[0])
	for _, r := range sorted[1:] {
		last := &merged[len(merged)-1]
		if r.Start <= last.End+1 {
			if r.End > last.End {
				last.End = r.End
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}
