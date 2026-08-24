package config

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"time"
)

// Descriptor is the declarative configuration contract used by documentation,
// surface checks, and `config check`. Parsing remains in the typed loaders.
type Descriptor struct {
	Name             string
	Product          string
	Type             string
	Default          string
	CommunityDefault string
	CloudDefault     string
	Secret           bool
	Required         string
	Description      string
}

// EffectiveValues returns non-secret shared values for human-readable checks.
func (c Config) EffectiveValues() map[string]string {
	return map[string]string{
		"HTTP_ADDR": c.HTTPAddr, "INTERNAL_ADDR": c.InternalAddr,
		"DRIVER_CACHE": c.Drivers.Cache, "DRIVER_RATELIMIT": c.Drivers.RateLimit,
		"DRIVER_BREAKER": c.Drivers.Breaker, "DRIVER_LOCK": c.Drivers.Lock,
		"TRUST_PROXY": strconv.FormatBool(c.TrustProxy), "TRUST_PROXY_HOPS": strconv.Itoa(c.TrustProxyHops),
		"RATE_LIMIT_PER_IP_RPM":      strconv.Itoa(c.RateLimitPerIPRPM),
		"AUTH_RATE_LIMIT_PER_IP_RPM": strconv.Itoa(c.AuthRateLimitPerIPRPM),
		"RATE_LIMIT_DISABLED":        strconv.FormatBool(c.RateLimitDisabled),
		"DRAIN_GRACE_SECONDS":        strconv.FormatInt(int64(c.DrainGrace/time.Second), 10),
		"SHUTDOWN_TIMEOUT_SECONDS":   strconv.FormatInt(int64(c.ShutdownTimeout/time.Second), 10),
		"DB_POOL_MAX_CONNS": func() string {
			if c.DBPoolMaxConns == 0 {
				return "<pgx default>"
			}
			return strconv.FormatInt(int64(c.DBPoolMaxConns), 10)
		}(),
	}
}

// WriteCheck prints a deterministic, redacted effective configuration. The
// loader must have succeeded before this is called.
func WriteCheck(w io.Writer, src Source, descriptors []Descriptor, values map[string]string, features map[string]bool) error {
	descriptors = append([]Descriptor(nil), descriptors...)
	sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].Name < descriptors[j].Name })
	if _, err := fmt.Fprintln(w, "configuration: valid"); err != nil {
		return err
	}
	for _, d := range descriptors {
		value, hasEffective := values[d.Name]
		source := "default"
		direct, _ := src.Lookup(d.Name)
		file, _ := src.Lookup(d.Name + "_FILE")
		if d.Secret {
			switch {
			case file != "":
				value, source = "<redacted>", "file"
			case direct != "":
				value, source = "<redacted>", "environment"
			case value == "":
				value, source = "<unset>", "unset"
			}
		} else if direct != "" {
			source = "environment"
			if !hasEffective {
				value = direct
			}
		} else if value == "" {
			value = d.Default
			if value == "" {
				value, source = "<unset>", "unset"
			}
		}
		if _, err := fmt.Fprintf(w, "%s=%s [%s]\n", d.Name, value, source); err != nil {
			return err
		}
	}
	if len(features) > 0 {
		if _, err := fmt.Fprintln(w, "features:"); err != nil {
			return err
		}
		names := make([]string, 0, len(features))
		for name := range features {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if _, err := fmt.Fprintf(w, "%s=%t\n", name, features[name]); err != nil {
				return err
			}
		}
	}
	return nil
}

var SharedDescriptors = []Descriptor{
	{Name: "DATABASE_URL", Product: "shared", Type: "url", Secret: true, Required: "runtime and database commands", Description: "PostgreSQL connection string."},
	{Name: "REDIS_URL", Product: "shared", Type: "url", Secret: true, Required: "when any shared driver uses redis", Description: "Redis connection string."},
	{Name: "SECRET_KEY", Product: "shared", Type: "hex", Secret: true, Required: "Cloud outside dev; Community persists one when absent", Description: "64-hex-character AES-256 master key."},
	{Name: "HTTP_ADDR", Product: "shared", Type: "address", Default: DefaultHTTPAddr, Description: "Public HTTP listen address."},
	{Name: "INTERNAL_ADDR", Product: "shared", Type: "address", Default: DefaultInternalAddr, Description: "Internal health and metrics listen address."},
	{Name: "DRIVER_CACHE", Product: "shared", Type: "enum", Default: DriverMemory, Description: "Cache driver: memory or redis."},
	{Name: "DRIVER_RATELIMIT", Product: "shared", Type: "enum", Default: DriverMemory, Description: "Rate-limit driver: memory or redis."},
	{Name: "DRIVER_BREAKER", Product: "shared", Type: "enum", Default: DriverMemory, Description: "Circuit-breaker driver: memory or redis."},
	{Name: "DRIVER_LOCK", Product: "shared", Type: "enum", Default: DriverMemory, Description: "Distributed lock driver: memory or redis."},
	{Name: "TRUST_PROXY", Product: "shared", Type: "bool", Default: "false", Description: "Trust forwarded client and scheme headers."},
	{Name: "TRUST_PROXY_HOPS", Product: "shared", Type: "int32", Default: "1", Description: "Number of trusted reverse-proxy hops."},
	{Name: "RATE_LIMIT_PER_IP_RPM", Product: "shared", Type: "int32", Default: "300", Description: "General requests per IP per minute."},
	{Name: "AUTH_RATE_LIMIT_PER_IP_RPM", Product: "shared", Type: "int32", Default: "10", Description: "Authentication requests per IP per minute."},
	{Name: "RATE_LIMIT_DISABLED", Product: "shared", Type: "bool", Default: "false", Description: "Explicitly disable all rate limiting."},
	{Name: "DRAIN_GRACE_SECONDS", Product: "shared", Type: "seconds", Default: "product-specific", CommunityDefault: "0", CloudDefault: "0 (dev), 3 (staging/production)", Description: "Delay between readiness failure and server shutdown."},
	{Name: "SHUTDOWN_TIMEOUT_SECONDS", Product: "shared", Type: "seconds", Default: "320", Description: "Total graceful server shutdown budget."},
	{Name: "DB_POOL_MAX_CONNS", Product: "shared", Type: "int32", Default: "pgx default", Description: "Maximum PostgreSQL pool size; unset delegates to pgx."},
}

// ExpandedDescriptors adds the conventional NAME_FILE contract for every
// secret and returns a stable, duplicate-checked name order.
func ExpandedDescriptors(groups ...[]Descriptor) ([]Descriptor, error) {
	byName := make(map[string]Descriptor)
	for _, group := range groups {
		for _, d := range group {
			if d.Name == "" || d.Description == "" || d.Type == "" || d.Product == "" {
				return nil, fmt.Errorf("config metadata: incomplete descriptor for %q", d.Name)
			}
			if _, exists := byName[d.Name]; exists {
				return nil, fmt.Errorf("config metadata: duplicate descriptor %s", d.Name)
			}
			byName[d.Name] = d
			if d.Secret {
				file := d
				file.Name += "_FILE"
				file.Type = "file"
				file.Default = ""
				file.Required = "alternative to " + d.Name
				file.Description = "Path containing " + d.Name + "; mutually exclusive with the direct value."
				if _, exists := byName[file.Name]; exists {
					return nil, fmt.Errorf("config metadata: duplicate descriptor %s", file.Name)
				}
				byName[file.Name] = file
			}
		}
	}
	out := make([]Descriptor, 0, len(byName))
	for _, d := range byName {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
