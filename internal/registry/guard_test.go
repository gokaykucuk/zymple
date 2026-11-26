package registry

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gokaykucuk/zymple/internal/auth"
)

func TestAuthorize(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		method, path string
		want         bool
	}{
		{"GET", "/v2/", true},
		{"HEAD", "/v2", true},
		{"POST", "/v2/", false},
		{"POST", "/v2/zymple/shop/blobs/uploads/", true},
		{"POST", "/v2/zymple/shop/blobs/uploads", true},
		{"PATCH", "/v2/zymple/shop/blobs/uploads/some-id", true},
		{"PUT", "/v2/zymple/shop/blobs/uploads/some-id", true},
		{"GET", "/v2/zymple/shop/blobs/uploads/some-id", true},
		{"DELETE", "/v2/zymple/shop/blobs/uploads/some-id", true},
		{"HEAD", "/v2/zymple/shop/blobs/" + digest, true},
		{"GET", "/v2/zymple/shop/blobs/" + digest, false},
		{"DELETE", "/v2/zymple/shop/blobs/" + digest, false},
		{"HEAD", "/v2/zymple/shop/manifests/latest", true},
		{"PUT", "/v2/zymple/shop/manifests/latest", true},
		{"GET", "/v2/zymple/shop/manifests/latest", false},
		{"DELETE", "/v2/zymple/shop/manifests/latest", false},
		{"GET", "/v2/zymple/shop/tags/list", false},
		{"GET", "/v2/_catalog", false},
		{"POST", "/v2/zymple/other/blobs/uploads/", false},
		{"HEAD", "/v2/zymple/other/blobs/" + digest, false},
		{"PUT", "/v2/zymple/shop-evil/manifests/latest", false},
		{"PUT", "/v2/shop/manifests/latest", false},
		{"PUT", "/v2/library/postgres/manifests/17", false},
		{"PUT", "/v2/zymple/shop/../other/manifests/latest", false},
		{"PUT", "/v2/zymple/shop//manifests/latest", false},
		{"PUT", "/v2/zymple/shop/manifests/a/b", false},
		{"PUT", "/v2/zymple/shop/manifests/", false},
		{"HEAD", "/v2/zymple/shop/blobs/", false},
		{"GET", "/", false},
		{"GET", "/v2/zymple/shop", false},
		{"TRACE", "/v2/zymple/shop/blobs/uploads/x", false},
	}
	for _, c := range cases {
		if got := Authorize("shop", c.method, c.path); got != c.want {
			t.Errorf("Authorize(shop, %s, %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

func newGuard(t *testing.T) (*Guard, *http.Request, string) {
	t.Helper()
	line, _ := auth.HashLine("ci", "zym_good")
	set, err := auth.Parse([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	var seen *http.Request
	g := &Guard{
		Tokens: func(app string) (auth.Set, bool) {
			if app == "shop" {
				return set, true
			}
			return nil, false
		},
	}
	holder := &http.Request{}
	g.Next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		*holder = *r
		w.WriteHeader(http.StatusAccepted)
	})
	_ = seen
	return g, holder, "zym_good"
}

func TestGuard(t *testing.T) {
	g, passed, token := newGuard(t)
	do := func(method, target, user, pass string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		return rec
	}

	rec := do("GET", "/v2/", "", "")
	if rec.Code != 401 || !strings.Contains(rec.Header().Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("no credentials: %d %v", rec.Code, rec.Header())
	}
	if !strings.Contains(rec.Body.String(), "UNAUTHORIZED") {
		t.Errorf("error body is not in registry format: %s", rec.Body.String())
	}
	for name, creds := range map[string][2]string{
		"wrong token":      {"shop", "zym_bad"},
		"unknown app":      {"ghost", token},
		"invalid app name": {"Shop!", token},
		"empty password":   {"shop", ""},
	} {
		if rec := do("GET", "/v2/", creds[0], creds[1]); rec.Code != 401 {
			t.Errorf("%s: got %d, want 401", name, rec.Code)
		}
	}
	if rec := do("GET", "/v2/", "shop", token); rec.Code != http.StatusAccepted {
		t.Errorf("valid credentials: got %d", rec.Code)
	}
	if rec := do("POST", "/v2/zymple/other/blobs/uploads/", "shop", token); rec.Code != 403 || !strings.Contains(rec.Body.String(), "DENIED") {
		t.Errorf("other repository: got %d %s", rec.Code, rec.Body.String())
	}

	rec = do("POST", "/v2/zymple/shop/blobs/uploads/?mount=sha256:abc&from=zymple/other&keep=1", "shop", token)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("mount request: got %d", rec.Code)
	}
	q := passed.URL.Query()
	if q.Has("mount") || q.Has("from") || q.Get("keep") != "1" {
		t.Errorf("mount parameters were not stripped: %s", passed.URL.RawQuery)
	}
	if strings.Contains(passed.RequestURI, "mount") {
		t.Errorf("RequestURI still carries the mount: %s", passed.RequestURI)
	}
}

func FuzzAuthorize(f *testing.F) {
	f.Add("PUT", "/v2/zymple/shop/manifests/latest")
	f.Add("GET", "/v2/zymple/shop/../other/blobs/x")
	f.Add("POST", "/v2/zymple/shopx/blobs/uploads/")
	f.Fuzz(func(t *testing.T, method, path string) {
		if !Authorize("shop", method, path) {
			return
		}
		if path == "/v2" || path == "/v2/" {
			return
		}
		if !strings.HasPrefix(path, "/v2/zymple/shop/") || strings.Contains(path, "..") || strings.Contains(path, "//") {
			t.Fatalf("authorized a path outside the app's repository: %s %q", method, path)
		}
		if method == "GET" && !strings.Contains(path, "/blobs/uploads") {
			t.Fatalf("authorized a read: %q", path)
		}
	})
}
