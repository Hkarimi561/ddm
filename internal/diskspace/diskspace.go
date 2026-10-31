// Package diskspace reports free disk space.
package diskspace

// Free returns the bytes available to this user on the filesystem holding
// path, which must exist.
func Free(path string) (uint64, error) { return free(path) }
