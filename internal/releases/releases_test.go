package releases

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	st, err := s.Load("shop")
	if err != nil || st.Next != 1 || st.Current != 0 || len(st.Releases) != 0 {
		t.Fatalf("empty state = %+v, %v", st, err)
	}
	if _, ok := st.CurrentRelease(); ok {
		t.Error("an empty state has no current release")
	}
	st.Releases = append(st.Releases, Release{Number: 1, Digest: "sha256:a", Port: 20000, Time: time.Unix(100, 0).UTC(), By: "ci"})
	st.Current, st.Next, st.LastError = 1, 2, "boom"
	if err := s.Save("shop", st); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load("shop")
	if err != nil {
		t.Fatal(err)
	}
	cur, ok := got.CurrentRelease()
	if !ok || cur.Digest != "sha256:a" || cur.By != "ci" || got.Next != 2 || got.LastError != "boom" {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if _, ok := got.Find(9); ok {
		t.Error("Find returned a release that does not exist")
	}
	info, err := os.Stat(filepath.Join(s.Dir("shop"), "releases.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %v, %v", info.Mode(), err)
	}
	entries, _ := os.ReadDir(s.Dir("shop"))
	if len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}

func TestStoreLoadErrors(t *testing.T) {
	s := &Store{Root: t.TempDir()}
	path := filepath.Join(s.Dir("shop"), "releases.json")
	if err := WriteFileAtomic(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("shop"); err == nil {
		t.Error("corrupt state should be an error, not an empty state")
	}
	if err := WriteFileAtomic(path, []byte(`{"current":0,"next":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := s.Load("shop")
	if err != nil || st.Next != 1 {
		t.Errorf("Next should be repaired to 1: %+v, %v", st, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("shop"); err == nil {
		t.Error("an unreadable state file should be an error")
	}
	if err := s.Save("shop", &State{}); err == nil {
		t.Error("saving over a directory should fail")
	}
}

func TestWriteFileAtomicErrors(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(filepath.Join(blocker, "child", "x"), []byte("x"), 0o600); err == nil {
		t.Error("writing below a regular file should fail")
	}
}
