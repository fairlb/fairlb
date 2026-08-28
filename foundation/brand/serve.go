package brand

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
)

// Surface names the app one build serves. It selects the display name and the
// web manifest; nothing else about a brand varies by surface.
type Surface string

const (
	SurfaceConsole        Surface = "console"
	SurfaceOperations     Surface = "operations"
	SurfaceCommunityAdmin Surface = "communityAdmin"
)

// The holes a production build leaves in index.html. They are written by
// packages/brand/src/build.ts and must match it character for character;
// TestPlaceholdersMatchTheBuildPlugin reads that file and pins them.
const (
	titlePlaceholder       = "__FLB_BRAND_TITLE__"
	canvasLightPlaceholder = "__FLB_BRAND_CANVAS_LIGHT__"
	canvasDarkPlaceholder  = "__FLB_BRAND_CANVAS_DARK__"
	profileJSONPlaceholder = "__FLB_BRAND_PROFILE_JSON__"
)

// profilePath is where both the artifact and a mounted bundle keep the profile.
const profilePath = "brand/profile.json"

// Serving is one SPA build with a brand applied.
type Serving struct {
	// FS is the build to hand to httpx.SPA. It is the embedded one with the
	// brand's files shadowing it and index.html already rendered.
	FS fs.FS
	// Name is identity.name of the profile actually being served, or "" when
	// there was no profile to read (no build embedded and no bundle mounted).
	Name string
}

// Serve applies a brand to one SPA build.
//
// dir is the mounted bundle: the contents of `/brand`, as produced by
// public/web/scripts/pack-brand-profile.mjs. Empty means the artifact's own
// default brand, which is a complete profile in its own right -- that is why
// there is no "is a bundle mounted" branch past this point. Anything the bundle
// provides shadows the artifact; anything it omits falls through.
//
// **It is fail-closed on purpose.** A named directory that is missing, is not a
// bundle, or leaves an asset its profile references unresolvable is an error,
// and callers are expected to refuse to start on it. The browser bundle falls
// back to the default profile when a page carries no island, so a deployment
// that half-loaded its brand would otherwise serve the default silently -- the
// exact failure the build-time input was chosen to prevent (ADR-0147), which is
// why the check has to live here rather than in the page.
func Serve(dist fs.FS, dir string, surface Surface) (Serving, error) {
	files, err := bundleFiles(dir)
	if err != nil {
		return Serving{}, err
	}
	raw, err := readThrough(files, dist, profilePath)
	if err != nil {
		if dir == "" && dist == nil {
			// Dev without an embedded build and without a bundle: there is no
			// profile anywhere and none is required. httpx.SPA renders its own
			// placeholder for a nil build.
			return Serving{}, nil
		}
		return Serving{}, fmt.Errorf("brand: read %s: %w", profilePath, err)
	}
	var p profile
	if err := json.Unmarshal(raw, &p); err != nil {
		return Serving{}, fmt.Errorf("brand: parse %s: %w", profilePath, err)
	}
	if err := p.check(surface); err != nil {
		return Serving{}, err
	}
	// Everything the profile names has to come from the same place the profile
	// did. Letting a mounted bundle fall through to the artifact for one missing
	// face would serve this brand's pages in the default brand's typeface --
	// which reads as a rendering bug and sends the wrong person looking.
	for _, ref := range p.assetRefs() {
		// One place, not both: a mounted bundle answers for its own assets and
		// the artifact answers for its own. Passing both is what let a bundle
		// missing a face pass by borrowing the default one.
		lookup, base := files, fs.FS(nil)
		if dir == "" {
			lookup, base = nil, dist
		}
		if _, err := readThrough(lookup, base, strings.TrimPrefix(ref, "/")); err != nil {
			return Serving{}, fmt.Errorf("brand: %s names %s, which the bundle does not have: %w",
				profilePath, ref, err)
		}
	}
	if dist == nil {
		// A bundle with no build to serve it: the name is still wanted, for mail
		// and the authenticator issuer.
		return Serving{Name: p.Identity.Name}, nil
	}

	manifest, err := p.manifest(surface)
	if err != nil {
		return Serving{}, err
	}
	files["site.webmanifest"] = manifest
	index, err := readThrough(files, dist, "index.html")
	if err != nil {
		return Serving{}, fmt.Errorf("brand: read index.html: %w", err)
	}
	files["index.html"] = p.render(index, surface, raw)
	return Serving{FS: overlay{files: files, base: dist}, Name: p.Identity.Name}, nil
}

// bundleFiles reads a mounted bundle into memory, keyed by served path.
//
// A brand is a few kilobytes of text plus four font files, read once at
// startup; holding it is cheaper than keeping a second filesystem open and
// removes every question about the directory changing under a running process.
func bundleFiles(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	if dir == "" {
		return files, nil
	}
	root := os.DirFS(dir)
	if err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		body, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		files[path.Join("brand", p)] = body
		return nil
	}); err != nil {
		return nil, fmt.Errorf("brand: read the bundle at %s: %w", dir, err)
	}
	for _, required := range []string{profilePath, "brand/profile.css"} {
		if _, ok := files[required]; !ok {
			return nil, fmt.Errorf("brand: the bundle at %s has no %s; "+
				"build it with public/web/scripts/pack-brand-profile.mjs", dir, path.Base(required))
		}
	}
	return files, nil
}

func readThrough(files map[string][]byte, base fs.FS, name string) ([]byte, error) {
	if body, ok := files[name]; ok {
		return body, nil
	}
	if base == nil {
		return nil, fs.ErrNotExist
	}
	return fs.ReadFile(base, name)
}

// profile is the part of BrandProfileV1 the server needs. It is deliberately
// not the whole schema: the profile is validated when the bundle is packed, and
// a second Go implementation of those rules would be a second thing to keep
// true (ADR-0153). What is here is what gets read.
type profile struct {
	Identity struct {
		Name         string `json:"name"`
		ShortName    string `json:"shortName"`
		SurfaceNames map[string]struct {
			En string `json:"en"`
		} `json:"surfaceNames"`
		Assets struct {
			FaviconSvg    string `json:"faviconSvg"`
			MarkSvg       string `json:"markSvg"`
			WordmarkSvg   string `json:"wordmarkSvg"`
			SocialMarkSvg string `json:"socialMarkSvg"`
		} `json:"assets"`
	} `json:"identity"`
	Theme struct {
		Light struct {
			Canvas string `json:"canvas"`
			Accent string `json:"accent"`
		} `json:"light"`
		Dark struct {
			Canvas string `json:"canvas"`
		} `json:"dark"`
		Fonts map[string]json.RawMessage `json:"fonts"`
	} `json:"theme"`
}

func (p profile) check(surface Surface) error {
	switch {
	case p.Identity.Name == "":
		return fmt.Errorf("brand: %s has no identity.name", profilePath)
	case p.Identity.ShortName == "":
		return fmt.Errorf("brand: %s has no identity.shortName", profilePath)
	case p.Theme.Light.Canvas == "" || p.Theme.Dark.Canvas == "" || p.Theme.Light.Accent == "":
		return fmt.Errorf("brand: %s has no theme colours", profilePath)
	}
	if p.surfaceName(surface) == "" {
		return fmt.Errorf("brand: %s has no identity.surfaceNames.%s", profilePath, surface)
	}
	return nil
}

func (p profile) surfaceName(surface Surface) string {
	return p.Identity.SurfaceNames[string(surface)].En
}

// assetRefs is every file the profile points at. Checking them is what turns
// "the profile parsed" into "the pages it describes can actually be served":
// a bundle missing one font answers 404 for it and falls back to a system face,
// which looks like a rendering bug rather than a deployment one.
func (p profile) assetRefs() []string {
	refs := []string{
		p.Identity.Assets.FaviconSvg,
		p.Identity.Assets.MarkSvg,
		p.Identity.Assets.WordmarkSvg,
		p.Identity.Assets.SocialMarkSvg,
	}
	for _, raw := range p.Theme.Fonts {
		var face struct {
			Sources []struct {
				Path string `json:"path"`
			} `json:"sources"`
		}
		// chineseFallback is a list of family names, not a face; it simply has
		// no sources and contributes nothing here.
		if err := json.Unmarshal(raw, &face); err != nil {
			continue
		}
		for _, source := range face.Sources {
			refs = append(refs, source.Path)
		}
	}
	out := refs[:0]
	for _, ref := range refs {
		if strings.HasPrefix(ref, "/brand/") {
			out = append(out, ref)
		}
	}
	return out
}

// webManifest mirrors what the build plugin used to emit. It is generated here
// rather than shipped in the bundle because `name` is the *surface* name and one
// bundle serves every surface at once.
type webManifest struct {
	Name            string            `json:"name"`
	ShortName       string            `json:"short_name"`
	StartURL        string            `json:"start_url"`
	Display         string            `json:"display"`
	BackgroundColor string            `json:"background_color"`
	ThemeColor      string            `json:"theme_color"`
	Icons           []webManifestIcon `json:"icons"`
}

type webManifestIcon struct {
	Src     string `json:"src"`
	Sizes   string `json:"sizes"`
	Type    string `json:"type"`
	Purpose string `json:"purpose"`
}

func (p profile) manifest(surface Surface) ([]byte, error) {
	body, err := json.Marshal(webManifest{
		Name:            p.surfaceName(surface),
		ShortName:       p.Identity.ShortName,
		StartURL:        "/",
		Display:         "browser",
		BackgroundColor: p.Theme.Light.Canvas,
		ThemeColor:      p.Theme.Light.Accent,
		Icons: []webManifestIcon{{
			Src:     p.Identity.Assets.FaviconSvg,
			Sizes:   "any",
			Type:    "image/svg+xml",
			Purpose: "any",
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("brand: build the web manifest: %w", err)
	}
	return body, nil
}

// render fills the four holes the build left in index.html.
//
// The profile goes in as the bytes that were read, not as a re-marshalled copy:
// what the page carries is then exactly what the operator installed, and the
// island cannot drift from profile.json through a struct that only models part
// of it.
func (p profile) render(index []byte, surface Surface, raw []byte) []byte {
	var island bytes.Buffer
	if err := json.Compact(&island, raw); err != nil {
		island.Reset()
		island.Write(raw)
	}
	// `<` only ever appears inside a JSON string, and `\u003c` is that same
	// character written a way that cannot close the island. The bytes came off
	// disk untouched, so nothing has escaped them yet.
	escaped := strings.ReplaceAll(island.String(), "<", `\u003c`)
	// The other three land in element text and attribute values, so they are
	// escaped for HTML. A brand name is operator-supplied, not attacker-supplied
	// -- but it arrives as configuration, and configuration that can rewrite the
	// page it is describing is a hole regardless of who is expected to fill it.
	return []byte(strings.NewReplacer(
		titlePlaceholder, html.EscapeString(p.surfaceName(surface)),
		canvasLightPlaceholder, html.EscapeString(p.Theme.Light.Canvas),
		canvasDarkPlaceholder, html.EscapeString(p.Theme.Dark.Canvas),
		profileJSONPlaceholder, escaped,
	).Replace(string(index)))
}

// overlay is the embedded build with a handful of files replaced.
type overlay struct {
	files map[string][]byte
	base  fs.FS
}

func (o overlay) Open(name string) (fs.File, error) {
	if body, ok := o.files[name]; ok {
		return &memFile{name: path.Base(name), Reader: bytes.NewReader(body)}, nil
	}
	return o.base.Open(name)
}

// Stat keeps fs.Stat off the Open path for overridden files. httpx.SPA stats
// every request before serving it, and without this each one would allocate a
// reader just to be closed again.
func (o overlay) Stat(name string) (fs.FileInfo, error) {
	if body, ok := o.files[name]; ok {
		return memInfo{name: path.Base(name), size: int64(len(body))}, nil
	}
	return fs.Stat(o.base, name)
}

type memFile struct {
	name string
	*bytes.Reader
}

func (f *memFile) Stat() (fs.FileInfo, error) {
	return memInfo{name: f.name, size: f.Reader.Size()}, nil
}
func (f *memFile) Close() error { return nil }

type memInfo struct {
	name string
	size int64
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) Mode() fs.FileMode  { return 0o444 }
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return false }
func (i memInfo) Sys() any           { return nil }

var (
	_ fs.FS     = overlay{}
	_ fs.StatFS = overlay{}
	_ io.Seeker = (*memFile)(nil)
	_ fs.File   = (*memFile)(nil)
)
