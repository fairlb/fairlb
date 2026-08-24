package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) MapSource { return MapSource{Values: m} }

// devSecretKeyHex is a 64-hex-character master key for tests.
var devSecretKeyHex = strings.Repeat("ab", 32)

func TestLoadDefaults(t *testing.T) {
	cfg, err := LoadRuntime(env(nil), SelfHostedDefaults())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	for _, d := range []string{cfg.Drivers.Cache, cfg.Drivers.RateLimit, cfg.Drivers.Breaker, cfg.Drivers.Lock} {
		if d != DriverMemory {
			t.Errorf("driver = %q, want memory", d)
		}
	}
	if cfg.TrustProxyHops != 1 {
		t.Errorf("TrustProxyHops default = %d, want 1 (one proxy in front)", cfg.TrustProxyHops)
	}
}

// TRUST_PROXY_HOPS must be an integer of at least 1; zero, negatives and
// non-numbers refuse to start. Getting it wrong puts the rate-limit key on the
// wrong X-Forwarded-For entry, which is worse than not starting.
func TestLoadTrustProxyHops(t *testing.T) {
	cfg, err := LoadRuntime(env(map[string]string{"TRUST_PROXY_HOPS": "2"}), SelfHostedDefaults())
	if err != nil || cfg.TrustProxyHops != 2 {
		t.Errorf("TRUST_PROXY_HOPS=2 should take effect: cfg=%d err=%v", cfg.TrustProxyHops, err)
	}
	for _, bad := range []string{"0", "-1", "two", "1.5"} {
		if _, err := LoadRuntime(env(map[string]string{"TRUST_PROXY_HOPS": bad}), SelfHostedDefaults()); err == nil {
			t.Errorf("TRUST_PROXY_HOPS=%q should be refused", bad)
		}
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := LoadRuntime(env(map[string]string{
		"HTTP_ADDR":    ":9000",
		"DATABASE_URL": "postgres://x",
		"DRIVER_CACHE": "redis",
		"REDIS_URL":    "redis://localhost:6379",
		"SECRET_KEY":   devSecretKeyHex,
	}), SelfHostedDefaults())

	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":9000" || cfg.Drivers.Cache != DriverRedis {
		t.Errorf("the overrides did not take effect: %+v", cfg)
	}
	if cfg.Drivers.RateLimit != DriverMemory {
		t.Errorf("fields that were not overridden should keep their defaults: %+v", cfg.Drivers)
	}
}

func TestLoadRejectsInvalid(t *testing.T) {
	cases := []map[string]string{
		{"DRIVER_CACHE": "memcached"},
		{"DRIVER_LOCK": "redis"}, // no REDIS_URL
	}
	for _, c := range cases {
		if _, err := LoadRuntime(env(c), SelfHostedDefaults()); err == nil {
			t.Errorf("Load(%v) should have returned an error", c)
		}
	}
}

func TestLoadSecretKey(t *testing.T) {
	if _, err := LoadRuntime(env(map[string]string{"SECRET_KEY": "not-hex"}), SelfHostedDefaults()); err == nil {
		t.Error("a non-hex SECRET_KEY should be refused")
	}
	if _, err := LoadRuntime(env(map[string]string{"SECRET_KEY": "abcd"}), SelfHostedDefaults()); err == nil {
		t.Error("a too-short key should be refused")
	}
	cfg, err := LoadRuntime(env(map[string]string{"SECRET_KEY": devSecretKeyHex}), SelfHostedDefaults())
	if err != nil || len(cfg.SecretKey) != 32 {
		t.Fatalf("a valid SECRET_KEY should decode to 32 bytes: %v", err)
	}
	if cfg, err := LoadRuntime(env(nil), SelfHostedDefaults()); err != nil || cfg.SecretKey != nil {
		t.Errorf("the development default should be empty: %v %v", cfg.SecretKey, err)
	}
}

func TestLoadRateLimits(t *testing.T) {
	cfg, err := LoadRuntime(env(map[string]string{
		"RATE_LIMIT_PER_IP_RPM": "0", "RATE_LIMIT_DISABLED": "true",
	}), SelfHostedDefaults())

	if err != nil {
		t.Fatalf("the explicit switch should allow it: %v", err)
	}
	if !cfg.RateLimitDisabled || cfg.RateLimitPerIPRPM != 0 || cfg.AuthRateLimitPerIPRPM != 0 {
		t.Errorf("turning it off explicitly should zero both tiers: %+v", cfg)
	}
	// Product loaders decide whether a zero value is safe for their shape.
	if _, err := LoadRuntime(env(map[string]string{"RATE_LIMIT_PER_IP_RPM": "0"}), SelfHostedDefaults()); err != nil {
		t.Errorf("a zero rate limit should load in development: %v", err)
	}
	cfg, err = LoadRuntime(env(nil), SelfHostedDefaults())
	if err != nil || cfg.AuthRateLimitPerIPRPM != 10 {
		t.Errorf("the authentication tier should default to 10: %d %v", cfg.AuthRateLimitPerIPRPM, err)
	}
}

// Pool sizing: the memory cache's invalidation listener permanently holds one
// connection, so an explicit value must be at least 2.
func TestLoadDBPoolMaxConns(t *testing.T) {
	cfg, err := LoadRuntime(env(map[string]string{"DB_POOL_MAX_CONNS": "8"}), SelfHostedDefaults())
	if err != nil || cfg.DBPoolMaxConns != 8 {
		t.Fatalf("the pool cap should take effect: %d %v", cfg.DBPoolMaxConns, err)
	}
	if _, err := LoadRuntime(env(map[string]string{"DB_POOL_MAX_CONNS": "1"}), SelfHostedDefaults()); err == nil {
		t.Error("one connection with the in-process cache should be refused: the listener takes it and none is left")
	}
	if _, err := LoadRuntime(env(map[string]string{"DB_POOL_MAX_CONNS": "0"}), SelfHostedDefaults()); err == nil {
		t.Error("an explicit 0 should be refused")
	}
	cfg, err = LoadRuntime(env(map[string]string{
		"DB_POOL_MAX_CONNS": "1", "DRIVER_CACHE": "redis", "REDIS_URL": "redis://x",
	}), SelfHostedDefaults())

	if err != nil || cfg.DBPoolMaxConns != 1 {
		t.Errorf("one connection is fine with the shared cache: %v", err)
	}
	if cfg, _ := LoadRuntime(env(nil), SelfHostedDefaults()); cfg.DBPoolMaxConns != 0 {
		t.Errorf("unset should be 0, meaning the driver default: %d", cfg.DBPoolMaxConns)
	}
}

// Shutdown windows. Negative values and a zero shutdown timeout are refused:
// getting the direction wrong truncates in-flight requests.
func TestLoadShutdownWindows(t *testing.T) {
	cfg, err := LoadRuntime(env(map[string]string{}), SelfHostedDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DrainGrace != 0 || cfg.ShutdownTimeout != 320*time.Second {
		t.Fatalf("the self-hosted defaults should be 0s and 320s: %v/%v", cfg.DrainGrace, cfg.ShutdownTimeout)
	}

	cfg, err = LoadRuntime(env(map[string]string{
		"DRAIN_GRACE_SECONDS":      "5",
		"SHUTDOWN_TIMEOUT_SECONDS": "320",
	}), SelfHostedDefaults())

	if err != nil {
		t.Fatal(err)
	}
	if cfg.DrainGrace != 5*time.Second || cfg.ShutdownTimeout != 320*time.Second {
		t.Fatalf("the overrides did not take effect: %v/%v", cfg.DrainGrace, cfg.ShutdownTimeout)
	}

	// A zero drain grace is legitimate (a single instance with no proxy to
	// drain from); a zero shutdown window is not.
	if _, err := LoadRuntime(env(map[string]string{"DRAIN_GRACE_SECONDS": "0"}), SelfHostedDefaults()); err != nil {
		t.Fatalf("a zero drain grace should be legal: %v", err)
	}
	for _, bad := range []map[string]string{
		{"SHUTDOWN_TIMEOUT_SECONDS": "0"},
		{"SHUTDOWN_TIMEOUT_SECONDS": "-1"},
		{"DRAIN_GRACE_SECONDS": "-1"},
		{"DRAIN_GRACE_SECONDS": "abc"},
	} {
		if _, err := LoadRuntime(env(bad), SelfHostedDefaults()); err == nil {
			t.Fatalf("%v should refuse to load", bad)
		}
	}
}

func TestStrictBooleansAndIntegerBounds(t *testing.T) {
	for _, values := range []map[string]string{
		{"TRUST_PROXY": "truthy"},
		{"RATE_LIMIT_DISABLED": "yes"},
		{"DB_POOL_MAX_CONNS": "2147483648"},
		{"SHUTDOWN_TIMEOUT_SECONDS": "9223372037"},
	} {
		if _, err := LoadRuntime(env(values), SelfHostedDefaults()); err == nil {
			t.Errorf("invalid value was accepted: %v", values)
		}
	}
	if cfg, err := LoadRuntime(env(map[string]string{"TRUST_PROXY": "TRUE"}), SelfHostedDefaults()); err != nil || !cfg.TrustProxy {
		t.Fatalf("strconv.ParseBool-compatible value was rejected: %+v %v", cfg, err)
	}
}

func TestSecretFilesAndMinimalDatabaseLoad(t *testing.T) {
	src := MapSource{Values: map[string]string{"DATABASE_URL_FILE": "/db"}, Files: map[string][]byte{"/db": []byte("postgres://x\n")}}
	dbCfg, err := LoadDatabase(src)
	if err != nil || dbCfg.DatabaseURL != "postgres://x" {
		t.Fatalf("file-backed database URL: %+v %v", dbCfg, err)
	}
	conflict := MapSource{Values: map[string]string{"DATABASE_URL": "postgres://a", "DATABASE_URL_FILE": "/db"}, Files: src.Files}
	if _, err := LoadDatabase(conflict); err == nil {
		t.Fatal("direct and file-backed values must conflict")
	}
	if _, err := LoadDatabase(MapSource{Values: map[string]string{}}); err == nil {
		t.Fatal("database-only load accepted a missing DATABASE_URL")
	}
}

// NormalizeHost is the one form every host comparison agrees on. It stays in
// this package because the request-side guards that compare against it do
// (internal/shared/httpx), even though nothing here reads a hostname from the
// environment any more.
func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"Console.X.com.": "console.x.com",
		"  API.x.com  ":  "api.x.com",
		"":               "",
	} {
		if got := NormalizeHost(in); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}
