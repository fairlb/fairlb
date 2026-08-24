// Package config layers Community-only settings on top of the shared runtime
// configuration. Cookie security follows PUBLIC_URL and logging follows
// LOG_FORMAT.
package config

import (
	"fmt"
	"net/url"
	"strings"

	foundation "github.com/fairlb/fairlb/foundation/config"
)

const (
	DefaultPublicURL = "http://localhost:8080"
	DefaultDataDir   = "/data"
	setupTokenMinLen = 32
)

type Config struct {
	foundation.Config
	PublicURL     *url.URL
	Secure        bool
	DataDir       string
	AdminEmail    string
	AdminPassword string
	SetupToken    string
	ProbeTrace    bool
	LogFormat     string
}

// LoadRuntime parses and validates everything required by the Community server.
func LoadRuntime(src foundation.Source) (Config, error) {
	base, err := foundation.LoadRuntime(src, foundation.SelfHostedDefaults())
	if err != nil {
		return Config{}, err
	}
	if base.DatabaseURL == "" {
		return Config{}, fmt.Errorf("config: DATABASE_URL is required")
	}

	adminPassword, err := foundation.Secret(src, "FAIRLB_ADMIN_PASSWORD")
	if err != nil {
		return Config{}, err
	}
	setupToken, err := foundation.Secret(src, "FAIRLB_SETUP_TOKEN")
	if err != nil {
		return Config{}, err
	}
	probeTrace, err := foundation.Bool(src, "FAIRLB_PROBE_TRACE", false)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		Config:        base,
		DataDir:       foundation.String(src, "FAIRLB_DATA_DIR", DefaultDataDir),
		AdminEmail:    strings.TrimSpace(foundation.String(src, "FAIRLB_ADMIN_EMAIL", "")),
		AdminPassword: adminPassword,
		SetupToken:    setupToken,
		ProbeTrace:    probeTrace,
		LogFormat:     foundation.String(src, "LOG_FORMAT", "text"),
	}

	u, err := parseBaseURL("PUBLIC_URL", foundation.String(src, "PUBLIC_URL", DefaultPublicURL))
	if err != nil {
		return Config{}, err
	}
	cfg.PublicURL = u
	cfg.Secure = u.Scheme == "https"

	switch cfg.LogFormat {
	case "text", "json":
	default:
		return Config{}, fmt.Errorf("config: LOG_FORMAT must be text or json (got %q)", cfg.LogFormat)
	}
	if cfg.RateLimitPerIPRPM == 0 && !cfg.RateLimitDisabled {
		return Config{}, fmt.Errorf("config: RATE_LIMIT_PER_IP_RPM must be greater than 0; to disable rate limiting set RATE_LIMIT_DISABLED=true")
	}
	if cfg.AuthRateLimitPerIPRPM == 0 && !cfg.RateLimitDisabled {
		return Config{}, fmt.Errorf("config: AUTH_RATE_LIMIT_PER_IP_RPM must be greater than 0; to disable rate limiting set RATE_LIMIT_DISABLED=true")
	}
	if (cfg.AdminEmail == "") != (cfg.AdminPassword == "") {
		return Config{}, fmt.Errorf("config: FAIRLB_ADMIN_EMAIL and FAIRLB_ADMIN_PASSWORD must be set together")
	}
	if cfg.SetupToken != "" && len(cfg.SetupToken) < setupTokenMinLen {
		return Config{}, fmt.Errorf("config: FAIRLB_SETUP_TOKEN must be at least %d characters", setupTokenMinLen)
	}

	return cfg, nil
}

// LoadDatabase parses only the settings required by migrations and DB-only CLIs.
func LoadDatabase(src foundation.Source) (foundation.DatabaseConfig, error) {
	return foundation.LoadDatabase(src)
}

// LoadProbe parses only the address required by the healthcheck command.
func LoadProbe(src foundation.Source) foundation.ProbeConfig {
	return foundation.ProbeConfig{Addr: foundation.String(src, "HTTP_ADDR", foundation.DefaultHTTPAddr)}
}

func parseBaseURL(name, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" {
		return nil, fmt.Errorf("config: %s %q is not a valid HTTP(S) origin", name, raw)
	}
	if u.User != nil || u.ForceQuery || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("config: %s must not contain user info, a path, query, or fragment", name)
	}
	u.Path = ""
	return u, nil
}
