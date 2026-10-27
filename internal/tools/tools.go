//go:build tools

// Package tools pins dependencies that later tasks import. Keeping the blank
// imports here makes `go mod tidy` retain the pinned Hysteria modules even
// before the packages that use them exist.
package tools

import (
	_ "github.com/apernet/hysteria/core/v2/client"
	_ "github.com/apernet/hysteria/core/v2/server"
	_ "github.com/apernet/hysteria/extras/v2/auth"
	_ "github.com/apernet/hysteria/extras/v2/obfs"
)
