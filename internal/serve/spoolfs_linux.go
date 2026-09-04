//go:build linux

package serve

import "syscall"

// ramBacked reports whether path lives on a RAM-backed filesystem (tmpfs or
// ramfs), where "spooling to disk" actually consumes memory. Unknown or
// unreadable paths report false: this only gates a warning.
func ramBacked(path string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false
	}
	// Statfs_t.Type is int64 on 64-bit and int32 on 32-bit targets; compare the
	// low 32 bits so both build and RAMFS_MAGIC (which overflows int32) matches.
	const tmpfsMagic, ramfsMagic = 0x01021994, 0x858458f6
	typ := uint32(st.Type)
	return typ == tmpfsMagic || typ == ramfsMagic
}
