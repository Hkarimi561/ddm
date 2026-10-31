// Package uiauth stores the web UI's password (as a bcrypt hash, in a file
// readable only by the user) and keeps login sessions.
package uiauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// MinLength is the shortest password accepted.
const MinLength = 8

// PasswordPath is where the hash lives: next to the config file.
func PasswordPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "ui-password")
}

// Exists reports whether a password has been set.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Set stores a new password.
func Set(path, password string) error {
	if len(password) < MinLength {
		return fmt.Errorf("the password must have at least %d characters", MinLength)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, hash, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Check reports whether password is the stored one.
func Check(path, password string) (bool, error) {
	hash, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	err = bcrypt.CompareHashAndPassword([]byte(strings.TrimSpace(string(hash))), []byte(password))
	if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
		return false, nil
	}
	return err == nil, err
}

// Generate returns a random, readable password like "k7rq-2mfx-9bnd-p4ta".
func Generate() string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	s := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	return s[0:4] + "-" + s[4:8] + "-" + s[8:12] + "-" + s[12:16]
}

// Sessions keeps logged-in browser sessions in memory; restarting the
// server logs everyone out.
type Sessions struct {
	TTL time.Duration

	mu     sync.Mutex
	tokens map[string]time.Time // token -> expiry
}

// NewSessions returns a session store whose sessions last ttl.
func NewSessions(ttl time.Duration) *Sessions {
	return &Sessions{TTL: ttl, tokens: map[string]time.Time{}}
}

// Create starts a session and returns its token.
func (s *Sessions) Create() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[tok] = time.Now().Add(s.TTL)
	return tok
}

// Valid reports whether token belongs to a live session.
func (s *Sessions) Valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	ok := false
	for t, exp := range s.tokens {
		if now.After(exp) {
			delete(s.tokens, t)
			continue
		}
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			ok = true
		}
	}
	return ok
}

// Revoke ends one session; RevokeAll ends every session (after a password
// change).
func (s *Sessions) Revoke(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, token)
}

func (s *Sessions) RevokeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = map[string]time.Time{}
}

// Throttle slows down password guessing: after max failures within window,
// logins are refused until the window has passed.
type Throttle struct {
	Max    int
	Window time.Duration

	mu       sync.Mutex
	failures []time.Time
}

// Allowed reports whether a login attempt may proceed.
func (t *Throttle) Allowed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := time.Now().Add(-t.Window)
	kept := t.failures[:0]
	for _, f := range t.failures {
		if f.After(cutoff) {
			kept = append(kept, f)
		}
	}
	t.failures = kept
	return len(t.failures) < t.Max
}

// Fail records a failed attempt.
func (t *Throttle) Fail() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failures = append(t.failures, time.Now())
}
