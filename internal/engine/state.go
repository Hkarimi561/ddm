package engine

import (
	"encoding/json"
	"fmt"
	"os"
)

const stateVersion = 1

// State is the persisted progress of one blob download. It is rewritten
// atomically, and only ever claims bytes that were fsynced to the data file
// before it was written, so it never runs ahead of the data on disk.
type State struct {
	Version int          `json:"version"`
	Digest  string       `json:"digest"`
	Size    int64        `json:"size"`
	Chunks  []ChunkState `json:"chunks"`
}

// ChunkState is the persisted form of a chunk.
type ChunkState struct {
	Start int64  `json:"start"`
	End   int64  `json:"end"` // exclusive
	Done  int64  `json:"done"`
	Hash  []byte `json:"hash,omitempty"` // marshaled sha256 state after Done bytes
}

func loadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &st, nil
}

// validate checks that the state belongs to this blob and that its chunks
// tile [0, size) exactly.
func (s *State) validate(digest string, size int64) error {
	switch {
	case s.Version != stateVersion:
		return fmt.Errorf("state version %d, want %d", s.Version, stateVersion)
	case s.Digest != digest:
		return fmt.Errorf("state is for %s, not %s", s.Digest, digest)
	case s.Size != size:
		return fmt.Errorf("state size %d, want %d", s.Size, size)
	case len(s.Chunks) == 0:
		return fmt.Errorf("state has no chunks")
	}
	var next int64
	for i, c := range s.Chunks {
		if c.Start != next || c.End <= c.Start || c.Done < 0 || c.Done > c.End-c.Start {
			return fmt.Errorf("chunk %d is invalid: %+v", i, c)
		}
		next = c.End
	}
	if next != size {
		return fmt.Errorf("chunks cover %d bytes, want %d", next, size)
	}
	return nil
}

// ReadProgress reports how much of a partial download is done, from its
// state file.
func ReadProgress(statePath string) (done, size int64, digest string, err error) {
	st, err := loadState(statePath)
	if err != nil {
		return 0, 0, "", err
	}
	for _, c := range st.Chunks {
		done += c.Done
	}
	return done, st.Size, st.Digest, nil
}

// saveState writes the state through a temporary file and a rename, so a
// crash leaves either the old or the new state, never a torn one.
func saveState(path string, s *State) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
