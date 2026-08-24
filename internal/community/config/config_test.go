package config_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fairlb/fairlb/foundation/config"
	communityconfig "github.com/fairlb/fairlb/internal/community/config"
)

// env builds a source that starts from a working minimum, so each test only
// states the variable it is actually about.
func env(overrides map[string]string) config.MapSource {
	base := map[string]string{
		"DATABASE_URL": "postgres://u:p@localhost:5432/fairlb",
	}
	for k, v := range overrides {
		base[k] = v
	}
	return config.MapSource{Values: base}
}

func load(t *testing.T, overrides map[string]string) communityconfig.Config {
	t.Helper()
	cfg, err := communityconfig.LoadRuntime(env(overrides))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

// The zero-configuration case has to work: a bare `docker run` with only a
// database URL is the documented quick start.
func TestDefaultsAreEnoughToStart(t *testing.T) {
	cfg := load(t, nil)
	if got := cfg.PublicURL.String(); got != communityconfig.DefaultPublicURL {
		t.Errorf("PublicURL is %q, want %q", got, communityconfig.DefaultPublicURL)
	}
	if cfg.Secure {
		t.Error("Secure is true for an http:// public URL")
	}
	if cfg.DataDir != communityconfig.DefaultDataDir {
		t.Errorf("DataDir is %q, want %q", cfg.DataDir, communityconfig.DefaultDataDir)
	}
	if cfg.LogFormat != "text" {
		t.Errorf("LogFormat is %q, want text", cfg.LogFormat)
	}
	if cfg.ProbeTrace {
		t.Error("ProbeTrace defaults to on; internals should not be exposed by default")
	}
}

// Cookie security follows the URL people actually use, which is the whole
// reason the deployment tier went away.
func TestSecureFollowsThePublicURLScheme(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://llm.example.com", true},
		{"http://192.168.1.10:8080", false},
		{"https://example.com:8443/", true},
	} {
		cfg := load(t, map[string]string{"PUBLIC_URL": tc.url})
		if cfg.Secure != tc.want {
			t.Errorf("PUBLIC_URL=%s gave Secure=%v, want %v", tc.url, cfg.Secure, tc.want)
		}
	}
}

// Behind a TLS-terminating proxy the scheme on the wire is http, so the
// forwarded header decides — but only when the operator has said the proxy is
// trustworthy. Otherwise any client could ask for a Secure cookie that the
// browser then refuses to send back over the connection it actually has.
func TestPublicURLMustBeUsable(t *testing.T) {
	for _, bad := range []string{"llm.example.com", "ftp://example.com", "https://", "://"} {
		if _, err := communityconfig.LoadRuntime(env(map[string]string{"PUBLIC_URL": bad})); err == nil {
			t.Errorf("PUBLIC_URL=%q was accepted", bad)
		}
	}
}

func TestTrailingSlashIsTrimmedSoLinksDoNotDouble(t *testing.T) {
	cfg := load(t, map[string]string{"PUBLIC_URL": "https://example.com/"})
	if got := cfg.PublicURL.String() + "/setup"; got != "https://example.com/setup" {
		t.Fatalf("setup link is %q", got)
	}
}

// Fail-closed rate limiting, with an explicit way to turn it off. The shared
// loader only enforces this outside dev, and this build has no tiers.
func TestRateLimitIsFailClosedUnlessDisabledOnPurpose(t *testing.T) {
	if _, err := communityconfig.LoadRuntime(env(map[string]string{"RATE_LIMIT_PER_IP_RPM": "0"})); err == nil {
		t.Fatal("a zero rate limit was accepted without RATE_LIMIT_DISABLED")
	} else if !strings.Contains(err.Error(), "RATE_LIMIT_DISABLED") {
		t.Errorf("the error does not name the explicit way out: %v", err)
	}

	cfg := load(t, map[string]string{
		"RATE_LIMIT_PER_IP_RPM": "0", "RATE_LIMIT_DISABLED": "true",
	})
	if !cfg.RateLimitDisabled {
		t.Error("RATE_LIMIT_DISABLED=true did not take effect")
	}

	if _, err := communityconfig.LoadRuntime(env(map[string]string{"AUTH_RATE_LIMIT_PER_IP_RPM": "0"})); err == nil {
		t.Fatal("a zero auth rate limit was accepted without RATE_LIMIT_DISABLED")
	} else if !strings.Contains(err.Error(), "AUTH_RATE_LIMIT_PER_IP_RPM") {
		t.Errorf("the error names the wrong auth rate-limit variable: %v", err)
	}
}

func TestLogFormatIsValidated(t *testing.T) {
	if _, err := communityconfig.LoadRuntime(env(map[string]string{"LOG_FORMAT": "xml"})); err == nil {
		t.Fatal("LOG_FORMAT=xml was accepted")
	}
	if cfg := load(t, map[string]string{"LOG_FORMAT": "json"}); cfg.LogFormat != "json" {
		t.Error("LOG_FORMAT=json did not take effect")
	}
}

// The _FILE convention is how Docker and Kubernetes hand a secret to a
// container without putting it in the environment, where every `docker inspect`
// and every child process can read it.
func TestAdminPasswordCanComeFromAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pw")
	if err := os.WriteFile(path, []byte("from-a-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := env(map[string]string{
		"FAIRLB_ADMIN_EMAIL": "a@example.com", "FAIRLB_ADMIN_PASSWORD_FILE": path,
	})
	src.Files = map[string][]byte{path: []byte("from-a-file\n")}
	cfg, err := communityconfig.LoadRuntime(src)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// The trailing newline is an artefact of writing the file, not part of the
	// password — keeping it would make every sign-in fail with the right
	// password typed correctly.
	if cfg.AdminPassword != "from-a-file" {
		t.Fatalf("password is %q", cfg.AdminPassword)
	}

	// Ambiguous precedence is rejected so stale credentials cannot stay hidden.
	if _, err := communityconfig.LoadRuntime(env(map[string]string{
		"FAIRLB_ADMIN_EMAIL":    "a@example.com",
		"FAIRLB_ADMIN_PASSWORD": "direct", "FAIRLB_ADMIN_PASSWORD_FILE": path,
	})); err == nil {
		t.Fatal("direct and file-backed passwords were both accepted")
	}

	if _, err := communityconfig.LoadRuntime(env(map[string]string{
		"FAIRLB_ADMIN_PASSWORD_FILE": filepath.Join(dir, "missing"),
	})); err == nil {
		t.Error("a missing password file was ignored")
	}
}

func TestAdminEmailIsTrimmed(t *testing.T) {
	cfg := load(t, map[string]string{"FAIRLB_ADMIN_EMAIL": "  a@example.com \n", "FAIRLB_ADMIN_PASSWORD": "long-enough-password"})
	if cfg.AdminEmail != "a@example.com" {
		t.Fatalf("email is %q", cfg.AdminEmail)
	}
}

func TestCredentialGroupsAndSetupTokenFailClosed(t *testing.T) {
	for _, values := range []map[string]string{
		{"FAIRLB_ADMIN_EMAIL": "a@example.com"},
		{"FAIRLB_ADMIN_PASSWORD": "password"},
		{"FAIRLB_SETUP_TOKEN": strings.Repeat("x", 31)},
		{"FAIRLB_PROBE_TRACE": "yes"},
	} {
		if _, err := communityconfig.LoadRuntime(env(values)); err == nil {
			t.Errorf("partial or invalid configuration was accepted: %v", values)
		}
	}
}

func TestPublicURLIsAnOrigin(t *testing.T) {
	for _, bad := range []string{
		"https://user@example.com", "https://example.com/path",
		"https://example.com?q=1", "https://example.com?", "https://example.com/#fragment",
		"https://:443",
	} {
		if _, err := communityconfig.LoadRuntime(env(map[string]string{"PUBLIC_URL": bad})); err == nil {
			t.Errorf("PUBLIC_URL=%q was accepted", bad)
		}
	}
}

func TestCommandSpecificLoadsIgnoreUnrelatedRuntimeSettings(t *testing.T) {
	src := config.MapSource{Values: map[string]string{
		"DATABASE_URL": "postgres://x", "LOG_FORMAT": "invalid", "PUBLIC_URL": ":bad:",
	}}
	if _, err := communityconfig.LoadDatabase(src); err != nil {
		t.Fatalf("database load was blocked by unrelated runtime settings: %v", err)
	}
	probe := communityconfig.LoadProbe(config.MapSource{Values: map[string]string{"HTTP_ADDR": ":8123", "DATABASE_URL": "broken"}})
	if probe.Addr != ":8123" {
		t.Fatalf("probe address = %q", probe.Addr)
	}
}

func TestConfigCheckIsRedactedAndReportsSources(t *testing.T) {
	src := config.MapSource{Values: map[string]string{
		"DATABASE_URL":       "postgres://user:secret@example/db",
		"FAIRLB_ADMIN_EMAIL": "a@example.com", "FAIRLB_ADMIN_PASSWORD": "never-print-me",
	}}
	cfg, err := communityconfig.LoadRuntime(src)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := communityconfig.WriteCheck(&output, src, cfg); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if strings.Contains(text, "never-print-me") || strings.Contains(text, "postgres://") {
		t.Fatalf("config check leaked a secret:\n%s", text)
	}
	for _, want := range []string{"configuration: valid", "DATABASE_URL=<redacted> [environment]", "PUBLIC_URL=http://localhost:8080 [default]", "features:"} {
		if !strings.Contains(text, want) {
			t.Errorf("config check missing %q:\n%s", want, text)
		}
	}
}
