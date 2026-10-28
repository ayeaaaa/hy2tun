package firewall

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"hy2tun/internal/ports"
)

const backendEnv = "HY2TUN_FIREWALL_BACKEND"

type closerFuncs struct {
	mu    sync.Mutex
	fns   []func()
	once  sync.Once
	close error
}

func (c *closerFuncs) add(fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fns = append(c.fns, fn)
}

func (c *closerFuncs) Close() error {
	c.once.Do(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		for i := len(c.fns) - 1; i >= 0; i-- {
			c.fns[i]()
		}
		c.fns = nil
	})
	return c.close
}

func redirectRanges(ranges []ports.Range) []ports.Range {
	if len(ranges) == 0 {
		return nil
	}
	out := append([]ports.Range(nil), ranges...)
	if out[0].Start == out[0].End {
		out = out[1:]
	} else {
		out[0].Start++
	}
	return out
}

func formatRange(r ports.Range) string {
	if r.Start == r.End {
		return strconv.Itoa(r.Start)
	}
	return fmt.Sprintf("%d-%d", r.Start, r.End)
}

func formatRanges(ranges []ports.Range) string {
	parts := make([]string, 0, len(ranges))
	for _, r := range ranges {
		parts = append(parts, formatRange(r))
	}
	return strings.Join(parts, ",")
}

func ruleName(addr *net.UDPAddr, ranges []ports.Range) string {
	ip := ""
	port := 0
	if addr != nil {
		ip = addr.IP.String()
		port = addr.Port
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s", ip, port, formatRanges(ranges))))
	return hex.EncodeToString(sum[:6])
}
