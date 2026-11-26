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
	Number int    `json:"number"`
	Digest string `json:"digest"`
	// Images holds the digests of the app's extra images, by name. The
	// release pins them together with Digest, so a rollback brings back the
	// whole set.
	Images map[string]string `json:"images,omitempty"`
	Port   int               `json:"port"`
	Time   time.Time         `json:"time"`
	// By is the id of the token that deployed it, or "admin".
	By string `json:"by"`
	// RolledBackFrom is set when the release re-activates an older image.
	RolledBackFrom int `json:"rolled_back_from,omitempty"`
}

// Uses reports whether the digest is one of the release's images.
func (r Release) Uses(digest string) bool {
	if digest == "" {
		return false
	}
	if r.Digest == digest {
		return true
	}
	for _, d := range r.Images {
		if d == digest {
			return true
		}
	}
	return false
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
	// PendingTags records when cleanup first saw each image tag it could
	// not account for, usually a push that has not been deployed yet. Such
	// a tag is removed once it has been seen for a grace period.
	PendingTags map[string]time.Time `json:"pending_tags,omitempty"`
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
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// syncDir flushes a directory, which is what makes a rename in it survive a
// power loss. Without it the file's new content is on disk but its name may
// still point at the old one, and a release recorded as live, or a release
// number already handed out, would be forgotten.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
