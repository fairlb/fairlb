package catalog

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/fairlb/fairlb/settings"
)

// Setting keys in the gateway namespace. Keys are registered by the layer that
// owns them; the settings package provides only the mechanism. Registration
// happens in this package's init, so the keys exist only in a build that
// actually includes it.
const (
	// There is deliberately no global markup key. The only source of a model's
	// multiplier is its price row, and every model in the catalog necessarily
	// has one, so a global markup could never take effect. Leaving such a key
	// registered would show the operator a setting that renders as live while
	// the pricing path has stopped reading it -- a knob that turns and does
	// nothing is worse than no knob at all. The same applies to the retired
	// per-group markup: discounts moved onto the organization's settings row and
	// became multiplicative rather than an override.
	KeyFXUSDCNY   = "gateway.fx_usd_cny"
	KeyKillSwitch = "gateway.kill_switch"
	// The service fee for a organization using their own upstream credential. They
	// pay the upstream directly and only this fee is charged here: the charge
	// is the upstream cost times this rate times the exchange rate. The
	// default of 500 basis points is 5%.
	KeyBYOKFeeBps = "gateway.byok_fee_bps"
	// Stateful upstream response/interaction ids are retained only long enough
	// to route follow-up operations back to their creating credential.
	KeyResourceAffinityTTLDays = "gateway.resource_affinity_ttl_days"
	// There is likewise no pricing-engine-mode key: with a single set of
	// prices, "which set wins" is not a question. Its registration had to go
	// with it -- left in the registry, the settings page would still render a
	// three-way dropdown with no consumer behind it.
)

// The rate ceiling of 100000 basis points is 10x. It exists so that an extra
// zero is refused at write time instead of becoming a tenfold bill.
const maxFeeBps = 100000

// Specs is what this package contributes to the settings registry.
//
// 它是一个纯函数而不是 `init()`：注册表由装配点建（ADR-0194），于是「设置页上
// 有哪些键」是**装出来的那个 server 的性质**，不是「哪些包碰巧被链接了」的性质。
func Specs() []settings.Spec {
	var out []settings.Spec
	// Every billing key is ImpactHigh: each multiplies straight into a bill,
	// and one wrong digit is every organization's money.
	out = append(out, settings.Spec{
		Key: KeyFXUSDCNY, Kind: settings.KindDecimalString,
		// A default of "0" means not configured, not "the rate is zero". A
		// wallet in that currency then fails billing and leaves a record,
		// rather than silently computing the wrong amount from a
		// plausible-looking default.
		Description:    "USD to CNY exchange rate, as a decimal string. \"0\" means unconfigured, and billing in CNY will fail.",
		DescriptionKey: "settingDescFxUsdCny",
		Group:          settings.GroupBilling, Impact: settings.ImpactHigh,
		Default: json.RawMessage(`"0"`),
	})
	out = append(out, settings.Spec{
		Key: KeyBYOKFeeBps, Kind: settings.KindInt,
		Range:          &settings.Range{Min: 0, Max: maxFeeBps},
		Description:    "Service fee in basis points for organizations using their own upstream credentials. They pay the upstream directly; only this fee is charged here.",
		DescriptionKey: "settingDescByokFeeBps",
		Group:          settings.GroupBilling, Impact: settings.ImpactHigh,
		Default: json.RawMessage(`500`),
	})
	out = append(out, settings.Spec{
		Key: KeyKillSwitch, Kind: settings.KindBool,
		Description: "Global stop switch. When true the data plane refuses every request; this is the broadest of the three kill switches.",
		// The impact needs no explanation, but the Impact value here does not
		// drive a confirmation dialog: the settings page renders this key
		// read-only and points at the health page instead. A global stop
		// switch gets exactly one entry point.
		DescriptionKey: "settingDescKillSwitch",
		Group:          settings.GroupOperations, Impact: settings.ImpactHigh,
		Default: json.RawMessage(`false`),
	})
	out = append(out, settings.Spec{
		Key: KeyResourceAffinityTTLDays, Kind: settings.KindInt,
		Range:          &settings.Range{Min: 1, Max: 90},
		Description:    "Days to retain stateful upstream response and interaction route affinity.",
		DescriptionKey: "settingDescResourceAffinityTtlDays",
		Group:          settings.GroupRetention, Impact: settings.ImpactHigh,
		Default: json.RawMessage(`60`),
	})
	return out
}

// Settings reads the gateway.* settings, through the cache, with invalidation
// broadcast.
type Settings struct{ store *settings.Store }

func NewSettings(store *settings.Store) *Settings { return &Settings{store: store} }

// defaultBYOKFeeBps matches the registered default and is the fallback when the
// read fails.
const defaultBYOKFeeBps = 500

const defaultResourceAffinityTTLDays = 60

// ResourceAffinityTTL is bounded at the read site as well as the settings
// write site so a corrupt or manually edited value cannot create unbounded
// resource retention.
func (s *Settings) ResourceAffinityTTL(ctx context.Context) time.Duration {
	var days int64
	found, err := s.store.Get(ctx, KeyResourceAffinityTTLDays, &days)
	if err != nil {
		slog.ErrorContext(ctx, "failed to read resource affinity TTL, falling back to the default", "error", err)
	}
	if err != nil || !found || days < 1 || days > 90 {
		days = defaultResourceAffinityTTLDays
	}
	return time.Duration(days) * 24 * time.Hour
}

// BYOKFeeBps reads the service fee.
//
// A failed read falls back to the default rather than to 0, because 0 means
// this request is free. Settings reads usually fail transiently -- a cache or a
// connection -- and a transient failure should not become a revenue hole.
func (s *Settings) BYOKFeeBps(ctx context.Context) int64 {
	var v int64
	found, err := s.store.Get(ctx, KeyBYOKFeeBps, &v)
	if err != nil {
		slog.ErrorContext(ctx, "failed to read the BYOK service fee, falling back to the default", "error", err)
		return defaultBYOKFeeBps
	}
	if !found || v < 0 {
		return defaultBYOKFeeBps
	}
	return v
}

// FXRate returns the exchange rate against USD as a string; USD is always "1".
// When unconfigured ("0" or absent) it returns the empty string and the caller
// raises a configuration error: the exchange rate multiplies straight into a
// bill, so quietly substituting a default is quietly computing the wrong
// amount.
func (s *Settings) FXRate(ctx context.Context, currency string) string {
	if currency == "" || currency == "USD" {
		return "1"
	}
	if currency != "CNY" {
		return "" // no other currency is supported yet
	}
	var v string
	if found, err := s.store.Get(ctx, KeyFXUSDCNY, &v); err != nil || !found {
		if err != nil {
			slog.ErrorContext(ctx, "failed to read the FX rate", "error", err)
		}
		return ""
	}
	if v == "" || v == "0" {
		return ""
	}
	return v
}

// KillSwitchState reads the switch plus who last changed it, so the operator UI
// can show who pulled it and when.
//
// It is separate from KillSwitch because that one sits on every request's hot
// path, needs only a bool and goes through the cache, while this one serves the
// admin surface and wants the whole row from the underlying store.
func (s *Settings) KillSwitchState(ctx context.Context) (active bool, updatedAt time.Time, updatedBy string, err error) {
	entries, err := s.store.List(ctx)
	if err != nil {
		return false, time.Time{}, "", err
	}
	for _, e := range entries {
		if e.Spec.Key != KeyKillSwitch {
			continue
		}
		// Undecodable is treated as not pulled, which is the safe direction:
		// the banner is missing once, rather than a healthy gateway being
		// rendered as stopped.
		_ = json.Unmarshal(e.Value, &active)
		if e.Set {
			updatedAt, updatedBy = e.UpdatedAt, e.UpdatedBy
		}
		return active, updatedAt, updatedBy, nil
	}
	return false, time.Time{}, "", nil
}

// SetKillSwitch writes the global switch. The key and its value shape belong to
// this package, so the write goes through here too: letting callers marshal the
// bool and name the key themselves copies the shape of this key out of it.
func (s *Settings) SetKillSwitch(ctx context.Context, active bool, updatedBy string) error {
	raw, err := json.Marshal(active)
	if err != nil {
		return err
	}
	return s.store.Set(ctx, KeyKillSwitch, raw, updatedBy)
}

// KillSwitch reports whether the global switch is pulled. A failed read is
// treated as not pulled: this is an availability switch, not a security one,
// and stopping service because a setting could not be read turns one cache
// failure into a full outage.
func (s *Settings) KillSwitch(ctx context.Context) bool {
	var v bool
	if found, err := s.store.Get(ctx, KeyKillSwitch, &v); err != nil || !found {
		if err != nil {
			slog.ErrorContext(ctx, "failed to read the global kill switch, treating it as off", "error", err)
		}
		return false
	}
	return v
}
