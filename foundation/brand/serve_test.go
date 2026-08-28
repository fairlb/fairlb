package brand_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/fairlb/fairlb/foundation/brand"
)

// The four holes and the island id are a contract between three readers: the
// vite plugin writes them, brand.Serve fills them, and the artifact gate
// asserts a built index.html still has them. Only one of the three is Go, so
// the other two are pinned by reading the file that defines them -- a rename on
// that side otherwise shows up as pages that render "__FLB_BRAND_TITLE__".
func TestPlaceholdersMatchTheBuildPlugin(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "web", "packages", "brand", "src", "build.ts"))
	if err != nil {
		t.Fatalf("read the build plugin: %v", err)
	}
	for _, want := range []string{
		`export const BRAND_TITLE_PLACEHOLDER = "__FLB_BRAND_TITLE__";`,
		`export const BRAND_CANVAS_LIGHT_PLACEHOLDER = "__FLB_BRAND_CANVAS_LIGHT__";`,
		`export const BRAND_CANVAS_DARK_PLACEHOLDER = "__FLB_BRAND_CANVAS_DARK__";`,
		`export const BRAND_PROFILE_JSON_PLACEHOLDER = "__FLB_BRAND_PROFILE_JSON__";`,
	} {
		if !strings.Contains(string(source), want) {
			t.Errorf("build.ts no longer declares %s", want)
		}
	}
	id, err := os.ReadFile(filepath.Join("..", "..", "web", "packages", "brand", "src", "profile.ts"))
	if err != nil {
		t.Fatalf("read the profile module: %v", err)
	}
	if !strings.Contains(string(id), `export const RUNTIME_PROFILE_ELEMENT_ID = "brand-profile";`) {
		t.Error("profile.ts no longer declares the island id the server fills")
	}
}

const indexHTML = `<!doctype html>
<html lang="en">
  <head>
    <link rel="icon" href="/brand/favicon.svg" type="image/svg+xml" />
    <meta name="theme-color" content="__FLB_BRAND_CANVAS_LIGHT__" media="(prefers-color-scheme: light)" />
    <meta name="theme-color" content="__FLB_BRAND_CANVAS_DARK__" media="(prefers-color-scheme: dark)" />
    <title>__FLB_BRAND_TITLE__</title>
  <link rel="manifest" href="/site.webmanifest" />
  <link rel="stylesheet" href="/brand/profile.css" />
  <script type="application/json" id="brand-profile">__FLB_BRAND_PROFILE_JSON__</script>
  </head>
  <body><div id="root"></div></body>
</html>
`

func profileJSON(name, canvasLight, canvasDark string) string {
	return `{"profileVersion":1,"identity":{"name":"` + name + `","shortName":"` + name + `",` +
		`"surfaceNames":{"marketing":{"en":"` + name + `","zh":"` + name + `"},` +
		`"console":{"en":"` + name + ` Console","zh":"x"},` +
		`"operations":{"en":"` + name + ` Operations","zh":"x"},` +
		`"communityAdmin":{"en":"` + name + ` Admin","zh":"x"}},` +
		`"assets":{"wordmarkSvg":"/brand/wordmark.svg","markSvg":"/brand/mark.svg",` +
		`"faviconSvg":"/brand/favicon.svg","socialMarkSvg":"/brand/social-mark.svg"}},` +
		`"theme":{"light":{"canvas":"` + canvasLight + `","surface":"#FFFFFF","ink":"#111111",` +
		`"accent":"#2457E6","healthy":"#0B8F83","degraded":"#B86E00"},` +
		`"dark":{"canvas":"` + canvasDark + `","surface":"#131A24","ink":"#EEEEEE",` +
		`"accent":"#7EA2FF","healthy":"#4FD1C5","degraded":"#F4B860"},` +
		`"fonts":{"display":{"family":"D","sources":[{"path":"/brand/fonts/display-0.woff2","weight":"400","style":"normal"}]},` +
		`"body":{"family":"B","sources":[{"path":"/brand/fonts/body-0.woff2","weight":"400","style":"normal"}]},` +
		`"mono":{"family":"M","sources":[{"path":"/brand/fonts/mono-0.woff2","weight":"400","style":"normal"}]},` +
		`"chineseFallback":["PingFang SC"]}},` +
		`"operator":{"legalName":"` + name + `","supportEmail":"s@example.com"},` +
		`"links":{"repository":"https://example.com","deploymentDocs":"https://example.com/d"},` +
		`"marketing":{}}`
}

// artifact is a stand-in for an embedded SPA build: index.html with the holes in
// it, and a complete default brand at the paths a bundle would shadow.
func artifact() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                  {Data: []byte(indexHTML)},
		"brand/profile.json":          {Data: []byte(profileJSON("FairLB", "#F5F7FA", "#0B1018"))},
		"brand/profile.css":           {Data: []byte(":root{--flb-profile-light-canvas:#F5F7FA}")},
		"brand/wordmark.svg":          {Data: []byte("<svg/>")},
		"brand/mark.svg":              {Data: []byte("<svg/>")},
		"brand/favicon.svg":           {Data: []byte("<svg/>")},
		"brand/social-mark.svg":       {Data: []byte("<svg/>")},
		"brand/fonts/display-0.woff2": {Data: []byte("wOF2default")},
		"brand/fonts/body-0.woff2":    {Data: []byte("wOF2default")},
		"brand/fonts/mono-0.woff2":    {Data: []byte("wOF2default")},
		"assets/app-abc123.js":        {Data: []byte("console.log(1)")},
	}
}

// bundle writes a complete brand bundle, the shape pack-brand-profile.mjs emits.
func bundle(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("profile.json", profileJSON(name, "#F8F7FC", "#100B18"))
	write("profile.css", ":root{--flb-profile-light-canvas:#F8F7FC}")
	for _, asset := range []string{"wordmark.svg", "mark.svg", "favicon.svg", "social-mark.svg"} {
		write(asset, "<svg data-brand=\"x\"/>")
	}
	for _, face := range []string{"display-0", "body-0", "mono-0"} {
		write("fonts/"+face+".woff2", "wOF2bundle")
	}
	return dir
}

func read(t *testing.T, serving brand.Serving, name string) string {
	t.Helper()
	f, err := serving.FS.Open(name)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

// With nothing mounted the artifact's own brand is served, and it is served the
// same way a mounted one is -- the placeholders are filled either way. If they
// were only filled when a bundle existed, the default deployment would ship
// pages reading "__FLB_BRAND_TITLE__" and no test that mounts a brand would see
// it.
func TestDefaultBrandFillsTheSameHoles(t *testing.T) {
	serving, err := brand.Serve(artifact(), "", brand.SurfaceConsole)
	if err != nil {
		t.Fatal(err)
	}
	if serving.Name != "FairLB" {
		t.Errorf("Name = %q, want FairLB", serving.Name)
	}
	index := read(t, serving, "index.html")
	if strings.Contains(index, "__FLB_BRAND_") {
		t.Errorf("index.html still has an unfilled hole:\n%s", index)
	}
	for _, want := range []string{"<title>FairLB Console</title>", "#F5F7FA", "#0B1018"} {
		if !strings.Contains(index, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
	if body := read(t, serving, "brand/mark.svg"); body != "<svg/>" {
		t.Errorf("mark.svg = %q, want the artifact's own", body)
	}
}

func TestMountedBundleShadowsTheArtifact(t *testing.T) {
	serving, err := brand.Serve(artifact(), bundle(t, "YouModel AI"), brand.SurfaceConsole)
	if err != nil {
		t.Fatal(err)
	}
	if serving.Name != "YouModel AI" {
		t.Errorf("Name = %q", serving.Name)
	}
	index := read(t, serving, "index.html")
	for _, want := range []string{"<title>YouModel AI Console</title>", "#F8F7FC", "#100B18"} {
		if !strings.Contains(index, want) {
			t.Errorf("index.html lacks %q:\n%s", want, index)
		}
	}
	for _, unwanted := range []string{"FairLB", "#F5F7FA", "#0B1018"} {
		if strings.Contains(index, unwanted) {
			t.Errorf("index.html still leaks %q", unwanted)
		}
	}
	if body := read(t, serving, "brand/mark.svg"); !strings.Contains(body, "data-brand") {
		t.Errorf("mark.svg = %q, want the bundle's", body)
	}
	if body := read(t, serving, "brand/fonts/body-0.woff2"); body != "wOF2bundle" {
		t.Errorf("body font = %q, want the bundle's", body)
	}
	// Untouched by the brand, so it must still come from the artifact.
	if body := read(t, serving, "assets/app-abc123.js"); body != "console.log(1)" {
		t.Errorf("app chunk = %q", body)
	}
}

// The island carries the profile the operator installed, byte for byte, so the
// page and profile.json cannot disagree.
func TestIslandCarriesTheInstalledProfile(t *testing.T) {
	serving, err := brand.Serve(artifact(), bundle(t, "YouModel AI"), brand.SurfaceOperations)
	if err != nil {
		t.Fatal(err)
	}
	index := read(t, serving, "index.html")
	_, rest, ok := strings.Cut(index, `<script type="application/json" id="brand-profile">`)
	if !ok {
		t.Fatal("no island in the rendered index.html")
	}
	payload, _, ok := strings.Cut(rest, "</script>")
	if !ok {
		t.Fatal("island is not closed")
	}
	var parsed struct {
		Identity struct{ Name string } `json:"identity"`
	}
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		t.Fatalf("island is not JSON: %v\n%s", err, payload)
	}
	if parsed.Identity.Name != "YouModel AI" {
		t.Errorf("island identity.name = %q", parsed.Identity.Name)
	}
}

// A brand string containing markup must not be able to close the island.
func TestIslandEscapesMarkup(t *testing.T) {
	dir := bundle(t, "Acme")
	hostile := profileJSON(`</script><script>alert(1)</script>`, "#F8F7FC", "#100B18")
	if err := os.WriteFile(filepath.Join(dir, "profile.json"), []byte(hostile), 0o644); err != nil {
		t.Fatal(err)
	}
	serving, err := brand.Serve(artifact(), dir, brand.SurfaceConsole)
	if err != nil {
		t.Fatal(err)
	}
	index := read(t, serving, "index.html")
	if strings.Contains(index, "<script>alert(1)</script>") {
		t.Errorf("the island let markup through:\n%s", index)
	}
}

// The manifest is generated rather than shipped because its name is the surface
// name: one bundle serves the console and the operations app at once.
func TestManifestIsPerSurface(t *testing.T) {
	dir := bundle(t, "YouModel AI")
	for surface, want := range map[brand.Surface]string{
		brand.SurfaceConsole:    "YouModel AI Console",
		brand.SurfaceOperations: "YouModel AI Operations",
	} {
		serving, err := brand.Serve(artifact(), dir, surface)
		if err != nil {
			t.Fatal(err)
		}
		var manifest struct {
			Name       string                 `json:"name"`
			ThemeColor string                 `json:"theme_color"`
			Icons      []struct{ Src string } `json:"icons"`
		}
		if err := json.Unmarshal([]byte(read(t, serving, "site.webmanifest")), &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest.Name != want {
			t.Errorf("%s manifest name = %q, want %q", surface, manifest.Name, want)
		}
		if len(manifest.Icons) != 1 || manifest.Icons[0].Src != "/brand/favicon.svg" {
			t.Errorf("%s manifest icons = %+v", surface, manifest.Icons)
		}
	}
}

// Fail-closed. Each of these would otherwise be served as the default brand
// with nothing said, because the page falls back when it carries no profile.
func TestABundleThatWillNotLoadStopsStartup(t *testing.T) {
	t.Run("directory missing", func(t *testing.T) {
		if _, err := brand.Serve(artifact(), filepath.Join(t.TempDir(), "absent"), brand.SurfaceConsole); err == nil {
			t.Fatal("a missing bundle directory was accepted")
		}
	})
	t.Run("not a bundle", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("hi"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := mustFail(t, artifact(), dir)
		if !strings.Contains(err.Error(), "pack-brand-profile") {
			t.Errorf("error does not say how to build one: %v", err)
		}
	})
	t.Run("stylesheet missing", func(t *testing.T) {
		dir := bundle(t, "Acme")
		if err := os.Remove(filepath.Join(dir, "profile.css")); err != nil {
			t.Fatal(err)
		}
		mustFail(t, artifact(), dir)
	})
	t.Run("a face the profile names is absent", func(t *testing.T) {
		dir := bundle(t, "Acme")
		if err := os.Remove(filepath.Join(dir, "fonts", "body-0.woff2")); err != nil {
			t.Fatal(err)
		}
		// The artifact has a file at that path, but the bundle is the whole
		// brand: falling through would serve YouModel's page in FairLB's
		// typeface and look like a rendering bug rather than a deploy one.
		err := mustFail(t, artifact(), dir)
		if !strings.Contains(err.Error(), "body-0.woff2") {
			t.Errorf("error does not name the missing file: %v", err)
		}
	})
	t.Run("profile is not JSON", func(t *testing.T) {
		dir := bundle(t, "Acme")
		if err := os.WriteFile(filepath.Join(dir, "profile.json"), []byte("{"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustFail(t, artifact(), dir)
	})
	t.Run("profile is JSON but not a profile", func(t *testing.T) {
		dir := bundle(t, "Acme")
		if err := os.WriteFile(filepath.Join(dir, "profile.json"), []byte(`{"identity":{}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		mustFail(t, artifact(), dir)
	})
}

func mustFail(t *testing.T, dist fstest.MapFS, dir string) error {
	t.Helper()
	_, err := brand.Serve(dist, dir, brand.SurfaceConsole)
	if err == nil {
		t.Fatal("a broken bundle was accepted")
	}
	return err
}

// Development: no build embedded and no bundle is not an error, it is Tuesday.
func TestNoBuildAndNoBundleIsNotAnError(t *testing.T) {
	serving, err := brand.Serve(nil, "", brand.SurfaceConsole)
	if err != nil {
		t.Fatalf("dev startup failed: %v", err)
	}
	if serving.FS != nil || serving.Name != "" {
		t.Errorf("Serving = %+v, want zero", serving)
	}
}

// A bundle with no build to serve it still answers the one question the backend
// asks: what do we sign mail as.
func TestBundleWithoutABuildStillNamesTheBrand(t *testing.T) {
	serving, err := brand.Serve(nil, bundle(t, "YouModel AI"), brand.SurfaceOperations)
	if err != nil {
		t.Fatal(err)
	}
	if serving.Name != "YouModel AI" {
		t.Errorf("Name = %q", serving.Name)
	}
}

// http.FileServerFS is what actually serves this, and it needs files it can
// seek. An overlay file that only implements Read passes every test above and
// then answers 500 for the first request in production.
func TestTheOverlayServesOverHTTP(t *testing.T) {
	serving, err := brand.Serve(artifact(), bundle(t, "YouModel AI"), brand.SurfaceConsole)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.FileServerFS(serving.FS))
	defer server.Close()
	for path, want := range map[string]string{
		"/index.html":        "YouModel AI Console",
		"/brand/mark.svg":    "data-brand",
		"/site.webmanifest":  "YouModel AI Console",
		"/brand/profile.css": "#F8F7FC",
	} {
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d", path, resp.StatusCode)
			continue
		}
		if !strings.Contains(string(body), want) {
			t.Errorf("GET %s body lacks %q: %s", path, want, body)
		}
	}
}
