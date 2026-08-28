// Package config loads configuration shared by every FairLB build.
//
// Deployment-tier policy does not belong here. Cloud owns ENV and its
// production guards; Community owns its self-hosted defaults. This package
// parses only orthogonal infrastructure settings and product-provided default
// windows.
package config

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fairlb/fairlb/foundation/crypto"
)

// Environment names remain shared vocabulary for logging and tests. Only the
// Cloud loader reads ENV or attaches policy to these values.
const (
	EnvDev        = "dev"
	EnvStaging    = "staging"
	EnvProduction = "production"
)

const (
	DriverMemory = "memory"
	DriverRedis  = "redis"
)

const (
	DefaultHTTPAddr        = ":8080"
	DefaultInternalAddr    = ":9091"
	DefaultShutdownTimeout = 320 * time.Second
)

// Defaults contains product-owned defaults whose correct value depends on the
// deployment shape rather than the parser.
type Defaults struct {
	DrainGrace      time.Duration
	ShutdownTimeout time.Duration
}

// SelfHostedDefaults suit a single Community instance and Cloud development.
func SelfHostedDefaults() Defaults {
	return Defaults{DrainGrace: 0, ShutdownTimeout: DefaultShutdownTimeout}
}

// Config is the orthogonal configuration every running build consumes.
type Config struct {
	HTTPAddr     string
	InternalAddr string
	DatabaseURL  string
	RedisURL     string
	Drivers      Drivers

	TrustProxy     bool
	TrustProxyHops int

	RateLimitPerIPRPM     int
	AuthRateLimitPerIPRPM int
	RateLimitDisabled     bool

	// SecretKey is the decoded AES-256-GCM master key. Products decide whether
	// an absent key is legal in their deployment mode.
	SecretKey []byte

	DrainGrace      time.Duration
	ShutdownTimeout time.Duration

	// DBPoolMaxConns is 0 when pgx should choose its own default.
	DBPoolMaxConns int32

	// BrandProfileDir is the brand bundle to serve, as produced by
	// public/web/scripts/pack-brand-profile.mjs. Empty serves the brand built
	// into the artifact, which is a complete profile in its own right -- so
	// this is "which brand", never "whether a brand".
	//
	// A named directory that cannot be loaded is fatal rather than a fallback:
	// the browser bundle does fall back to the default profile when a page
	// carries no profile, so a half-loaded brand would otherwise be served as
	// the default with nothing said (foundation/brand.Serve).
	BrandProfileDir string
}

type Drivers struct {
	Cache     string
	RateLimit string
	Breaker   string
	Lock      string
}

// DatabaseConfig is the minimum configuration needed by migration and other
// database-only commands.
type DatabaseConfig struct {
	DatabaseURL    string
	DBPoolMaxConns int32
}

// ProbeConfig is the minimum configuration needed by an executable health
// probe. Product loaders choose the public or internal listener.
type ProbeConfig struct{ Addr string }

// LoadRuntime parses all shared runtime settings.
func LoadRuntime(src Source, defaults Defaults) (Config, error) {
	if defaults.ShutdownTimeout <= 0 {
		return Config{}, errors.New("config: product default ShutdownTimeout must be positive")
	}

	databaseURL, err := Secret(src, "DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	redisURL, err := Secret(src, "REDIS_URL")
	if err != nil {
		return Config{}, err
	}
	secretRaw, err := Secret(src, "SECRET_KEY")
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		HTTPAddr:        String(src, "HTTP_ADDR", DefaultHTTPAddr),
		InternalAddr:    String(src, "INTERNAL_ADDR", DefaultInternalAddr),
		DatabaseURL:     databaseURL,
		RedisURL:        redisURL,
		BrandProfileDir: String(src, "BRAND_PROFILE_DIR", ""),
		Drivers: Drivers{
			Cache:     String(src, "DRIVER_CACHE", DriverMemory),
			RateLimit: String(src, "DRIVER_RATELIMIT", DriverMemory),
			Breaker:   String(src, "DRIVER_BREAKER", DriverMemory),
			Lock:      String(src, "DRIVER_LOCK", DriverMemory),
		},
	}

	if cfg.TrustProxy, err = Bool(src, "TRUST_PROXY", false); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitDisabled, err = Bool(src, "RATE_LIMIT_DISABLED", false); err != nil {
		return Config{}, err
	}

	if cfg.RateLimitPerIPRPM, err = nonNegativeInt(src, "RATE_LIMIT_PER_IP_RPM", 300); err != nil {
		return Config{}, err
	}
	if cfg.AuthRateLimitPerIPRPM, err = nonNegativeInt(src, "AUTH_RATE_LIMIT_PER_IP_RPM", 10); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitDisabled {
		cfg.RateLimitPerIPRPM = 0
		cfg.AuthRateLimitPerIPRPM = 0
	}

	if cfg.TrustProxyHops, err = positiveInt(src, "TRUST_PROXY_HOPS", 1); err != nil {
		return Config{}, fmt.Errorf("%w (use 1 for one proxy, 2 when a CDN sits in front of it)", err)
	}

	if cfg.DrainGrace, err = seconds(src, "DRAIN_GRACE_SECONDS", defaults.DrainGrace, true); err != nil {
		return Config{}, err
	}
	if cfg.ShutdownTimeout, err = seconds(src, "SHUTDOWN_TIMEOUT_SECONDS", defaults.ShutdownTimeout, false); err != nil {
		return Config{}, err
	}

	if secretRaw != "" {
		key, err := crypto.DecodeKeyHex(secretRaw)
		if err != nil {
			return Config{}, fmt.Errorf("config: %w", err)
		}
		cfg.SecretKey = key
	}

	if cfg.DBPoolMaxConns, err = poolMaxConns(src); err != nil {
		return Config{}, err
	}

	for name, v := range map[string]string{
		"DRIVER_CACHE":     cfg.Drivers.Cache,
		"DRIVER_RATELIMIT": cfg.Drivers.RateLimit,
		"DRIVER_BREAKER":   cfg.Drivers.Breaker,
		"DRIVER_LOCK":      cfg.Drivers.Lock,
	} {
		if v != DriverMemory && v != DriverRedis {
			return Config{}, fmt.Errorf("config: %s %q is not valid (memory|redis)", name, v)
		}
	}
	if cfg.usesRedis() && cfg.RedisURL == "" {
		return Config{}, errors.New("config: a driver selected redis, so REDIS_URL is required")
	}
	if cfg.DBPoolMaxConns == 1 && cfg.Drivers.Cache == DriverMemory {
		return Config{}, errors.New("config: DB_POOL_MAX_CONNS=1 is too small: the in-process cache invalidation listener permanently holds one connection")
	}
	return cfg, nil
}

// LoadDatabase parses only database settings and requires a connection string.
// It intentionally does not apply the runtime memory-cache pool lower bound.
func LoadDatabase(src Source) (DatabaseConfig, error) {
	url, err := Secret(src, "DATABASE_URL")
	if err != nil {
		return DatabaseConfig{}, err
	}
	if url == "" {
		return DatabaseConfig{}, errors.New("config: DATABASE_URL is required")
	}
	maxConns, err := poolMaxConns(src)
	if err != nil {
		return DatabaseConfig{}, err
	}
	return DatabaseConfig{DatabaseURL: url, DBPoolMaxConns: maxConns}, nil
}

// Bool parses a Go boolean and rejects every non-empty invalid value.
func Bool(src Source, name string, def bool) (bool, error) {
	raw := String(src, name, "")
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("config: %s %q is not a valid boolean", name, raw)
	}
	return v, nil
}

func nonNegativeInt(src Source, name string, def int) (int, error) {
	raw := String(src, name, strconv.Itoa(def))
	v, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("config: %s %q is not a non-negative 32-bit integer", name, raw)
	}
	return int(v), nil
}

func positiveInt(src Source, name string, def int) (int, error) {
	v, err := nonNegativeInt(src, name, def)
	if err != nil || v < 1 {
		if err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("config: %s must be at least 1", name)
	}
	return v, nil
}

func poolMaxConns(src Source) (int32, error) {
	raw := String(src, "DB_POOL_MAX_CONNS", "")
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || v < 1 {
		return 0, fmt.Errorf("config: DB_POOL_MAX_CONNS %q is not a positive 32-bit integer", raw)
	}
	return int32(v), nil
}

func seconds(src Source, name string, def time.Duration, allowZero bool) (time.Duration, error) {
	if def < 0 || def%time.Second != 0 {
		return 0, fmt.Errorf("config: product default for %s must be a non-negative whole number of seconds", name)
	}
	raw := String(src, name, strconv.FormatInt(int64(def/time.Second), 10))
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 || v > math.MaxInt64/int64(time.Second) {
		return 0, fmt.Errorf("config: %s %q is not a representable non-negative number of seconds", name, raw)
	}
	if v == 0 && !allowZero {
		return 0, fmt.Errorf("config: %s must be greater than 0", name)
	}
	return time.Duration(v) * time.Second, nil
}

func (c Config) UsesRedis() bool { return c.usesRedis() }

func (c Config) usesRedis() bool {
	return slices.Contains(
		[]string{c.Drivers.Cache, c.Drivers.RateLimit, c.Drivers.Breaker, c.Drivers.Lock},
		DriverRedis,
	)
}

// NormalizeHost returns the canonical representation used by request guards.
func NormalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}
