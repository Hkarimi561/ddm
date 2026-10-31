package cli

import (
	"context"
	"testing"
	"time"
)

func TestParseAge(t *testing.T) {
	good := map[string]time.Duration{"": 0, "0": 0, "7d": 7 * 24 * time.Hour, "12h": 12 * time.Hour, "90m": 90 * time.Minute}
	for in, want := range good {
		if got, err := parseAge(in); err != nil || got != want {
			t.Errorf("parseAge(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"soon", "-1d", "xd", "-5h"} {
		if _, err := parseAge(bad); err == nil {
			t.Errorf("parseAge(%q) accepted", bad)
		}
	}
}

func TestWaitUntil(t *testing.T) {
	if err := waitUntil(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if err := waitUntil(context.Background(), "25:99"); err == nil {
		t.Error("an invalid time was accepted")
	}
	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	if err := waitUntil(context.Background(), past); err != nil {
		t.Errorf("a time in the past should start immediately: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	if err := waitUntil(ctx, future); err == nil {
		t.Error("waiting ignored cancellation")
	}
}
