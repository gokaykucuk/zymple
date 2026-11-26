package registry

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/gokaykucuk/zymple/internal/appdef"
	"github.com/gokaykucuk/zymple/internal/auth"
)

// TokenLookup returns the tokens accepted for an app, and whether it exists.
type TokenLookup func(app string) (auth.Set, bool)

// Guard authenticates registry requests with an app's token and limits each
// token to pushing into its own repository.
//
// The token is the password of HTTP basic auth and the app name is the user,
// which is what `docker login` sends.
type Guard struct {
	Next   http.Handler
	Tokens TokenLookup
}

func (g *Guard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	app, ok := g.authenticate(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="zymple"`)
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}
	if !Authorize(app, r.Method, r.URL.Path) {
		writeError(w, http.StatusForbidden, "DENIED", "this token may only push to "+appdef.Repository(app))
		return
	}
	stripMount(r)
	g.Next.ServeHTTP(w, r)
}

func (g *Guard) authenticate(r *http.Request) (string, bool) {
	user, pass, ok := r.BasicAuth()
	if !ok || !appdef.ValidName(user) {
		return "", false
	}
	set, known := g.Tokens(user)
	// Verify even for unknown apps so both cases cost the same.
	_, valid := set.Verify(pass)
	if !known || !valid {
		return "", false
	}
	return user, true
}

// Authorize decides whether a token for app may make a registry request.
// It allows exactly what a push needs:
//
//   - the API version check,
//   - blob existence checks and uploads in the app's own repository,
//   - manifest existence checks and uploads in the app's own repository.
//
// Reading blobs and manifests is denied. The content store is shared between
// apps, so a download route would let one app's token fetch another app's
// image by digest.
func Authorize(app, method, urlPath string) bool {
	if urlPath == "/v2" || urlPath == "/v2/" {
		return method == http.MethodGet || method == http.MethodHead
	}
	if urlPath != path.Clean(urlPath) && urlPath != path.Clean(urlPath)+"/" {
		return false
	}
	rest, ok := strings.CutPrefix(urlPath, "/v2/"+appdef.Repository(app)+"/")
	if !ok {
		return false
	}
	switch {
	case rest == "blobs/uploads" || strings.HasPrefix(rest, "blobs/uploads/"):
		switch method {
		case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodGet, http.MethodDelete:
			return true
		}
		return false
	case strings.HasPrefix(rest, "blobs/"):
		return method == http.MethodHead && isLeaf(rest, "blobs/")
	case strings.HasPrefix(rest, "manifests/"):
		return (method == http.MethodHead || method == http.MethodPut) && isLeaf(rest, "manifests/")
	}
	return false
}

func isLeaf(rest, prefix string) bool {
	leaf := strings.TrimPrefix(rest, prefix)
	return leaf != "" && !strings.Contains(leaf, "/")
}

// stripMount removes cross-repository mount parameters, turning a mount
// request into an ordinary upload, so a token cannot claim another
// repository's blob.
func stripMount(r *http.Request) {
	q := r.URL.Query()
	if !q.Has("mount") && !q.Has("from") {
		return
	}
	q.Del("mount")
	q.Del("from")
	u := *r.URL
	u.RawQuery = q.Encode()
	r.URL = &u
	r.RequestURI = (&url.URL{Path: u.Path, RawPath: u.RawPath, RawQuery: u.RawQuery}).RequestURI()
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]string{{"code": code, "message": message}},
	})
}
