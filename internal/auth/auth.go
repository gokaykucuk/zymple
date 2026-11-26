// Package auth verifies per-app deploy tokens. The host stores only SHA-256
// hashes; tokens are 256 bits of randomness, so a fast hash is sufficient.
package auth

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Prefix marks a string as a Zymple token, which helps secret scanners.
const Prefix = "zym_"

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Entry is one accepted token: an operator-chosen id and the token's hash.
type Entry struct {
	ID   string
	Hash [sha256.Size]byte
}

// Set is the tokens accepted for one app.
type Set []Entry

// Generate returns a new random token.
func Generate() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return Prefix + hex.EncodeToString(buf), nil
}

// HashLine returns the line to put in an app's tokens file.
func HashLine(id, token string) (string, error) {
	if !idRe.MatchString(id) {
		return "", fmt.Errorf("token id %q must match %s", id, idRe)
	}
	sum := sha256.Sum256([]byte(token))
	return id + " " + hex.EncodeToString(sum[:]), nil
}

// Parse reads a tokens file: one "<id> <sha256-hex>" per line. Blank lines
// and lines starting with "#" are ignored.
func Parse(data []byte) (Set, error) {
	var set Set
	seen := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 2 || !idRe.MatchString(fields[0]) {
			return nil, fmt.Errorf("line %d: want \"<id> <sha256-hex>\"", line)
		}
		raw, err := hex.DecodeString(fields[1])
		if err != nil || len(raw) != sha256.Size {
			return nil, fmt.Errorf("line %d: hash is not a SHA-256 hex digest", line)
		}
		if seen[fields[0]] {
			return nil, fmt.Errorf("line %d: duplicate token id %q", line, fields[0])
		}
		seen[fields[0]] = true
		var e Entry
		e.ID = fields[0]
		copy(e.Hash[:], raw)
		set = append(set, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return set, nil
}

// Load reads a tokens file. A missing file is an empty set: the app then
// accepts no token at all.
func Load(path string) (Set, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Verify returns the id of the entry matching token. Every entry is compared
// in constant time so timing reveals nothing about which one matched.
func (s Set) Verify(token string) (string, bool) {
	sum := sha256.Sum256([]byte(token))
	id, ok := "", false
	for _, e := range s {
		if subtle.ConstantTimeCompare(sum[:], e.Hash[:]) == 1 {
			id, ok = e.ID, true
		}
	}
	return id, ok
}
