//go:build windows

package config

// withConfigLock is a no-op on Windows. The config set command is only used on
// OpenWrt/Linux, where advisory flock protection is available.
func withConfigLock(_ string, fn func() error) error {
	return fn()
}
