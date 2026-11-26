// Package releases records which image each app runs. The record is a cache
// of what is on the host, kept in one JSON file per app.
package releases

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Release is one successful deploy.
type Release struct {
	Number int       `json:"number"`
	Digest string    `json:"digest"`
	Port   int       `json:"port"`
	Time   time.Time `json:"time"`
	// By is the id of the token that deployed it, or "admin".
	By string `json:"by"`
	// RolledBackFrom is set when the release re-activates an older image.
	RolledBackFrom int `json:"rolled_back_from,omitempty"`
}

// State is everything recorded for one app.
type State struct {
	// Current is the number of the release serving traffic, 0 if none.
	Current int `json:"current"`
	// Next is the number the next deploy attempt will use.
	Next     int       `json:"next"`
	Releases []Release `json:"releases"`
	// LastError describes the most recent failed deploy, "" after a success.
	LastError string `json:"last_error,omitempty"`
}

// CurrentRelease returns the release serving traffic.
func (s *State) CurrentRelease() (Release, bool) { return s.Find(s.Current) }

// Find returns the release with the given number.
func (s *State) Find(number int) (Release, bool) {
	for _, r := range s.Releases {
		if r.Number == number && number != 0 {
			return r, true
		}
	}
	return Release{}, false
}

// Store reads and writes release state under a root directory.
type Store struct {
	Root string
	mu   sync.Mutex
}

// Dir returns the state directory of an app.
func (s *Store) Dir(app string) string { return filepath.Join(s.Root, "apps", app) }

func (s *Store) path(app string) string { return filepath.Join(s.Dir(app), "releases.json") }

// Load returns the state of an app; a missing file is an empty state.
func (s *Store) Load(app string) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path(app))
	if errors.Is(err, os.ErrNotExist) {
		return &State{Next: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path(app), err)
	}
	if st.Next < 1 {
		st.Next = 1
	}
	return &st, nil
}

// Save writes the state atomically: a crash leaves the old file or the new
// one, never a partial write.
func (s *Store) Save(app string, st *State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(s.path(app), append(data, '\n'), 0o600)
}

// WriteFileAtomic writes data to path through a temporary file and rename.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
