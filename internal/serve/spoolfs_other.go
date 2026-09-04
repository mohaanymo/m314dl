//go:build !linux

package serve

// ramBacked is only detected on Linux (where /tmp is commonly tmpfs); other
// platforms never warn.
func ramBacked(string) bool { return false }
