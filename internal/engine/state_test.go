package engine

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	c := newChunk(0, 100)
	c.advance([]byte("some bytes"))
	want := &State{
		Version: stateVersion,
		Digest:  testDigest,
		Size:    200,
		Chunks:  []ChunkState{c.state(), {Start: 100, End: 200}},
	}
	if err := saveState(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if err := got.validate(testDigest, 200); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temporary state file left behind")
	}
}

func TestStateSaveReplacesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	for done := int64(0); done < 3; done++ {
		st := &State{Version: stateVersion, Digest: testDigest, Size: 10, Chunks: []ChunkState{{Start: 0, End: 10, Done: done}}}
		if err := saveState(path, st); err != nil {
			t.Fatal(err)
		}
	}
	got, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Chunks[0].Done != 2 {
		t.Fatalf("got done=%d, want the last saved value 2", got.Chunks[0].Done)
	}
}

func TestStateValidate(t *testing.T) {
	good := func() *State {
		return &State{Version: stateVersion, Digest: testDigest, Size: 10,
			Chunks: []ChunkState{{Start: 0, End: 4, Done: 4}, {Start: 4, End: 10, Done: 1}}}
	}
	if err := good().validate(testDigest, 10); err != nil {
		t.Fatalf("valid state rejected: %v", err)
	}
	tests := map[string]func(*State){
		"wrong version":  func(s *State) { s.Version = 99 },
		"wrong digest":   func(s *State) { s.Digest = "sha256:" + testDigest[8:12] },
		"wrong size":     func(s *State) { s.Size = 11 },
		"no chunks":      func(s *State) { s.Chunks = nil },
		"gap":            func(s *State) { s.Chunks[1].Start = 5 },
		"overlap":        func(s *State) { s.Chunks[1].Start = 3 },
		"short coverage": func(s *State) { s.Chunks[1].End = 9 },
		"done too large": func(s *State) { s.Chunks[0].Done = 5 },
		"negative done":  func(s *State) { s.Chunks[1].Done = -1 },
	}
	for name, mutate := range tests {
		s := good()
		mutate(s)
		if err := s.validate(testDigest, 10); err == nil {
			t.Errorf("%s: invalid state accepted", name)
		}
	}
}
