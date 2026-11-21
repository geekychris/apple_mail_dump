//go:build darwin

package snapshot

import "golang.org/x/sys/unix"

// cloneFile makes dst an APFS copy-on-write clone of src: instant, and no
// data is written until either file changes. It fails (ENOTSUP, EXDEV, ...)
// off APFS or across volumes; the caller then copies bytes.
func cloneFile(src, dst string) error {
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
}
