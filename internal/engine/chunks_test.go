package engine

import (
	"crypto/sha256"
	"reflect"
	"testing"
)

func TestPlanChunks(t *testing.T) {
	const mib = 1 << 20
	tests := []struct {
		name               string
		size               int64
		conns              int
		minChunk, maxChunk int64
		want               []span
	}{
		{"split by connections", 10, 4, 1, 100, []span{{0, 3}, {3, 6}, {6, 9}, {9, 10}}},
		{"min chunk wins", 1000, 4, 512, 1000, []span{{0, 512}, {512, 1000}}},
		{"small blob is one chunk", 100, 4, 512, 1000, []span{{0, 100}}},
		{"single connection", 10, 1, 1, 100, []span{{0, 10}}},
		{"zero connections means one", 10, 0, 1, 100, []span{{0, 10}}},
		{"max chunk caps size", 100 * mib, 4, mib, 16 * mib, []span{
			{0, 16 * mib}, {16 * mib, 32 * mib}, {32 * mib, 48 * mib}, {48 * mib, 64 * mib},
			{64 * mib, 80 * mib}, {80 * mib, 96 * mib}, {96 * mib, 100 * mib},
		}},
		{"empty", 0, 4, 1, 100, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := planChunks(tt.size, tt.conns, tt.minChunk, tt.maxChunk); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPlanChunksTilesTheBlob(t *testing.T) {
	for size := int64(1); size < 5000; size += 37 {
		for conns := 1; conns <= 8; conns++ {
			var next int64
			for _, s := range planChunks(size, conns, 16, 700) {
				if s.start != next || s.end <= s.start || s.end-s.start > 700 {
					t.Fatalf("size %d conns %d: bad span %v", size, conns, s)
				}
				next = s.end
			}
			if next != size {
				t.Fatalf("size %d conns %d: spans end at %d", size, conns, next)
			}
		}
	}
}

func TestParseContentRange(t *testing.T) {
	tests := []struct {
		in                 string
		first, last, total int64
		ok                 bool
	}{
		{"bytes 0-99/1000", 0, 99, 1000, true},
		{"bytes 500-999/*", 500, 999, -1, true},
		{"bytes 7-7/8", 7, 7, 8, true},
		{"bytes 9-3/10", 0, 0, 0, false},
		{"bytes */1000", 0, 0, 0, false},
		{"items 0-1/2", 0, 0, 0, false},
		{"", 0, 0, 0, false},
	}
	for _, tt := range tests {
		first, last, total, err := parseContentRange(tt.in)
		if (err == nil) != tt.ok {
			t.Errorf("%q: err = %v, want ok=%v", tt.in, err, tt.ok)
			continue
		}
		if tt.ok && (first != tt.first || last != tt.last || total != tt.total) {
			t.Errorf("%q: got %d-%d/%d", tt.in, first, last, total)
		}
	}
}

func TestChunkHashSurvivesRestart(t *testing.T) {
	c := newChunk(0, 10)
	c.advance([]byte("hello"))
	restored := chunkFromState(c.state())
	if restored.doneBytes() != 5 || restored.cursor() != 5 {
		t.Fatalf("restored chunk has done=%d cursor=%d", restored.doneBytes(), restored.cursor())
	}
	restored.advance([]byte("world"))
	want := sha256.Sum256([]byte("helloworld"))
	if got := restored.sum(); string(got) != string(want[:]) {
		t.Fatal("hash after resume differs from hashing the bytes in one go")
	}
	if !restored.complete() {
		t.Fatal("chunk should be complete")
	}
}

func TestChunkWithBadHashStartsOver(t *testing.T) {
	c := chunkFromState(ChunkState{Start: 0, End: 10, Done: 5, Hash: []byte("garbage")})
	if c.doneBytes() != 0 {
		t.Fatalf("chunk with unusable hash state kept %d bytes", c.doneBytes())
	}
}
