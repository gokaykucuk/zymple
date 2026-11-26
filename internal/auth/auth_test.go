package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateHashVerify(t *testing.T) {
	token, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	other, _ := Generate()
	if !strings.HasPrefix(token, Prefix) || len(token) != len(Prefix)+64 || token == other {
		t.Fatalf("unexpected tokens %q %q", token, other)
	}
	line1, err := HashLine("ci", token)
	if err != nil {
		t.Fatal(err)
	}
	line2, _ := HashLine("laptop", other)
	set, err := Parse([]byte("# deploy tokens\n\n" + line1 + "\n  " + line2 + "  \n"))
	if err != nil || len(set) != 2 {
		t.Fatalf("Parse = %v, %v", set, err)
	}
	if id, ok := set.Verify(token); !ok || id != "ci" {
		t.Errorf("Verify(token) = %q, %v", id, ok)
	}
	if id, ok := set.Verify(other); !ok || id != "laptop" {
		t.Errorf("Verify(other) = %q, %v", id, ok)
	}
	for _, bad := range []string{"", "zym_wrong", token + "x", strings.ToUpper(token)} {
		if _, ok := set.Verify(bad); ok {
			t.Errorf("Verify(%q) accepted", bad)
		}
	}
	if _, ok := Set(nil).Verify(token); ok {
		t.Error("an empty set accepted a token")
	}
	if strings.Contains(line1, token) {
		t.Error("the hash line contains the token itself")
	}
}

func TestHashLineRejectsBadIDs(t *testing.T) {
	for _, id := range []string{"", "has space", "UPPER", "-lead", strings.Repeat("a", 33)} {
		if _, err := HashLine(id, "t"); err == nil {
			t.Errorf("HashLine(%q) accepted", id)
		}
	}
}

func TestParseErrors(t *testing.T) {
	good := strings.Repeat("ab", 32)
	for name, data := range map[string]string{
		"one field":    "ci\n",
		"three fields": "ci " + good + " extra\n",
		"bad id":       "CI " + good + "\n",
		"not hex":      "ci " + strings.Repeat("zz", 32) + "\n",
		"short hash":   "ci abcd\n",
		"duplicate id": "ci " + good + "\nci " + good + "\n",
	} {
		if _, err := Parse([]byte(data)); err == nil {
			t.Errorf("%s: Parse accepted %q", name, data)
		}
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	set, err := Load(filepath.Join(dir, "missing"))
	if err != nil || set != nil {
		t.Fatalf("missing file: %v, %v", set, err)
	}
	line, _ := HashLine("ci", "secret")
	path := filepath.Join(dir, "tokens")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err = Load(path)
	if err != nil || len(set) != 1 {
		t.Fatalf("Load = %v, %v", set, err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("loading a directory should fail")
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte("ci " + strings.Repeat("ab", 32) + "\n"))
	f.Add([]byte("# comment\n\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		set, err := Parse(data)
		if err != nil {
			return
		}
		seen := map[string]bool{}
		for _, e := range set {
			if !idRe.MatchString(e.ID) || seen[e.ID] {
				t.Fatalf("accepted bad or duplicate id %q", e.ID)
			}
			seen[e.ID] = true
		}
	})
}
