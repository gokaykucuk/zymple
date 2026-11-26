package releases

import "testing"

func TestReleaseUses(t *testing.T) {
	r := Release{Digest: "sha256:a", Images: map[string]string{"ocr": "sha256:b"}}
	for digest, want := range map[string]bool{"sha256:a": true, "sha256:b": true, "sha256:c": false, "": false} {
		if got := r.Uses(digest); got != want {
			t.Errorf("Uses(%q) = %v, want %v", digest, got, want)
		}
	}
	if (Release{}).Uses("") {
		t.Error("an empty digest is never one of a release's images")
	}
}
