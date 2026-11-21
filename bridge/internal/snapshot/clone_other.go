//go:build !darwin

package snapshot

import "errors"

func cloneFile(src, dst string) error { return errors.New("clonefile: not supported on this OS") }
