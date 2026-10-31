//go:build !windows

package engine

import "os"

// makeSparse is a no-op: extending a file with Truncate already creates a
// sparse file on the Unix filesystems we care about.
func makeSparse(*os.File) error { return nil }
