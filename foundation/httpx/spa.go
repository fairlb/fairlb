package httpx

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// assetsDir is the bundler's default output directory: every fingerprinted
// build artifact lives under it and not a single client-side route does. That
// non-overlap is the entire basis for "do not fall back here, just 404".
const (
	assetsDir    = "assets"
	assetsPrefix = assetsDir + "/"
	// apiPrefix has no client-side routes either: every API plane is mounted
	// under /api/, and no client-side route of the SPA starts with it. Same
	// reasoning as assets — two namespaces that do not overlap, so the side
	// that does not overlap must not fall back into the other.
	apiPrefix = "api/"
)

var rootStaticFiles = map[string]struct{}{
	"apple-touch-icon.png": {},
	"favicon.ico":          {},
	"favicon.svg":          {},
	"robots.txt":           {},
	"site.webmanifest":     {},
}

// inAssets reports whether the request lands in the fingerprinted asset tree.
// The directory itself counts (path.Clean maps both /assets and /assets/ to
// "assets"): there is nothing servable there, and letting the file server list
// it hands out the full chunk manifest for free.
func inAssets(p string) bool {
	return p == assetsDir || strings.HasPrefix(p, assetsPrefix)
}

// inAPI reports whether the request lands in the API namespace.
//
// An unmatched API path must 404; it must never fall back to index.html.
// Falling back answers "this endpoint does not exist" with 200 + text/html:
//   - the caller gets HTML where it expected error JSON, and the parse blows up
//     somewhere far away from the actual mistake;
//   - monitoring does not even record a 4xx, so typo'd paths, endpoints that
//     were removed, and old clients still using a retired prefix all look like
//     a perfectly healthy service;
//   - it actively misleads during triage. A retired route prefix answering 200
//     reads as "the old routing is still live and the cutover did not take
//     effect"; the giveaway is that a completely invented path answers with the
//     same HTML.
//
// This is the classic "soft 404" problem; a SPA handler is just one more place
// it can be introduced.
func inAPI(p string) bool {
	return p == "api" || strings.HasPrefix(p, apiPrefix)
}

func isRootStaticFile(p string) bool {
	_, ok := rootStaticFiles[p]
	return ok
}

// SPA serves a single-page app build: static files are served directly
// (fingerprinted assets with a long cache), everything else falls back to
// index.html so the client-side router can take over. When no build is embedded
// it serves the placeholder text instead, which is the normal state during
// development where a dev server serves the app on another port.
//
// Two cache rules, each load-bearing:
//
//   - Fingerprinted files under assets/ are immutable, so they get a long cache;
//     a miss there is a 404 and never a client-route fallback. Falling back
//     would answer "that chunk is not part of this build any more" with
//     200 + text/html, which surfaces in the browser as a module-loading failure
//     and never appears as a 4xx anywhere. The concrete shape: after a deploy, a
//     tab still running the previous build navigates to a lazy route and gets a
//     kilobyte of index.html where a JavaScript chunk should be. The same holds
//     for /api/ — see inAPI.
//   - index.html is the one file that changes on every deploy, so it must be
//     no-cache (cacheable, but revalidated every time). The build is embedded in
//     the binary, and an embedded FS reports a zero ModTime, so the standard file
//     server sends neither Last-Modified nor ETag. Without an explicit validator
//     the freshness of the shell is left entirely to browser heuristics and there
//     is nothing to make a conditional request against — which would quietly
//     break any "your app shell is stale, reload" logic in the frontend, because
//     that logic assumes a reload really does fetch the new shell.
func SPA(dist fs.FS, placeholder string) http.Handler {
	if dist == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(placeholder))
		})
	}
	fileServer := http.FileServerFS(dist)
	notFound := NotFoundHandler()
	// The build cannot change while the process runs (it is embedded), so the
	// ETag only has to be computed once.
	indexETag := contentETag(dist, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		// A directory is treated like a miss: the assets directory itself has
		// nothing servable, and handing it to the file server lists the whole
		// chunk manifest.
		if info, err := fs.Stat(dist, p); err != nil || info.IsDir() {
			if inAssets(p) || inAPI(p) || isRootStaticFile(p) {
				// Neither namespace carries client-route meaning: not there
				// means not there.
				notFound(w, r)
				return
			}
			// Client-side route: rewrite to index.html.
			r.URL.Path = "/"
			p = "index.html"
		}
		switch {
		case strings.HasPrefix(p, assetsPrefix):
			// Fingerprinted, therefore immutable.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		case p == "index.html":
			w.Header().Set("Cache-Control", "no-cache")
			if indexETag != "" {
				// Set before delegating: http.ServeContent reads this very
				// header when evaluating conditional requests, so the standard
				// library answers 304 on its own and there is no If-None-Match
				// handling to write here.
				w.Header().Set("ETag", indexETag)
			}
		}
		fileServer.ServeHTTP(w, r)
	})
}

// contentETag computes a strong ETag over the file's bytes. If the file is not
// in the build it returns the empty string and the caller sends no ETag.
func contentETag(dist fs.FS, name string) string {
	b, err := fs.ReadFile(dist, name)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}
