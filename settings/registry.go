package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/fairlb/fairlb/foundation/money"
	"net/mail"
	"sort"
	"sync"
)

// The settings registry. Each layer contributes its own keys at assembly time,
// and the generic settings endpoint only accepts registered ones: writing an
// unregistered key is a 404, so the admin interface never becomes an arbitrary
// key-value write surface.
//
// # Why the registry is a value and not a package-level map
//
// It used to be a package global written by three `init()` functions across two
// modules. That made **the link set decide what the settings page renders**: a
// package that stops being imported takes its keys off the page, and there is no
// outward sign — the page simply has fewer rows, all of them correct.
//
// It is not the same situation as `errcode`, where init registration is right:
// there a missing code fails at the moment of use, loudly. Here the consumer is
// a rendered list, and absence is indistinguishable from "there was never such a
// setting" (ADR-0194).
//
// A global also makes the obvious criterion untestable: "an unassembled server
// shows no settings, an assembled one shows each one" cannot be asserted when
// the test binary links every package that registers.

// Kind is the type of a setting's value. It decides both the validation and the
// control the frontend renders.
type Kind string

const (
	KindString     Kind = "string"
	KindEnum       Kind = "enum"
	KindStringList Kind = "string_list"
	KindEmail      Kind = "email"
	KindBool       Kind = "bool"
	// KindInt is an integer; a non-nil Range constrains it.
	KindInt Kind = "int"
	// KindDecimalString is a decimal number carried as a *string*, e.g. an
	// exchange rate "7.15" or a unit price. Money and rates never touch
	// floating point, so the value travels as text and each consumer parses it
	// into a scaled integer: a JSON number would round-trip through float64 and
	// lose precision in the decimals.
	KindDecimalString Kind = "decimal_string"
	// KindMapStringInt is a name-to-integer mapping, e.g. a pricing group to its
	// markup in basis points. A Range constrains each value.
	KindMapStringInt Kind = "map_string_int"
	// KindMoney is an amount of money in main units, carried as a *decimal
	// string* ("10.00") for the same reason as KindDecimalString. A Range on a
	// money key is in nano, so the registry keeps the "one zero too many" bound
	// for a value a person types in dollars; an empty string means not
	// configured. Operators type amounts — they should never have to multiply
	// by a billion, and the three keys that used to be nano integers each
	// needed a sentence of description explaining the conversion.
	KindMoney Kind = "money"
	// KindSecret is a credential: a string that is sealed under the
	// application master key before it reaches the table, and that never comes
	// back out through the settings surface. A read returns only whether it is
	// set and a display hint; the value itself is available to the process
	// through Store.Get alone. An empty string written to a secret clears it.
	KindSecret Kind = "secret"
)

// Group is the section a setting appears under in the admin interface.
//
// The grouping follows what an operator is trying to do when they come to change
// it, not the key's namespace prefix. The registry returns keys in lexical
// order, and a flat list in that order splits the three "how is this priced"
// keys apart by prefix and interleaves them with "how long is data kept".
type Group string

const (
	// GroupAccess is "who gets in": signup gating, social login switches, email
	// blocklists, terms version.
	GroupAccess Group = "access"
	// GroupBilling is "how is this priced": exchange rates, service fees,
	// billing engine settings.
	GroupBilling Group = "billing"
	// GroupOperations is "the switches and alarms once it is running": the kill
	// switch, alert recipients, anomaly thresholds.
	GroupOperations Group = "operations"
	// GroupRetention is "how long is data kept": purge and archive windows.
	GroupRetention Group = "retention"
)

// Impact is how far a change reaches, expressed as data rather than as a rule
// someone has to remember.
type Impact string

const (
	// ImpactNormal changes only internal data, or is trivially reversible.
	// It saves directly.
	ImpactNormal Impact = "normal"
	// ImpactHigh reaches beyond the admin interface itself: who may sign in at
	// all, where live traffic goes, what customers are billed, or whether data
	// physically survives. These keys require a confirmation and a mandatory
	// reason before the write.
	ImpactHigh Impact = "high"
)

// Range is the inclusive constraint for KindInt, KindMapStringInt and (in
// nano) KindMoney; a nil
// Spec.Range means unconstrained.
//
// It is a pointer rather than using "Min and Max both zero means unconstrained"
// as a sentinel. That sentinel cannot express a lower bound of zero, which is
// both legitimate and common: a key meaning "must not be negative" is written as
// Min 0, Max is left at its zero value, and the entire range check is skipped —
// so a negative floor can be stored, and every org's hourly spend then counts as
// "above the floor" and the anomaly alert fires for everyone.
//
// When a sentinel value collides with a legitimate one, nothing errors. The code
// just quietly stops doing its job.
type Range struct{ Min, Max int64 }

// Spec describes one registered key.
type Spec struct {
	Key  string
	Kind Kind
	// Description explains the key to developers and operators; it shows up in
	// logs and alerts, and as the fallback when the interface has no translated
	// string. It is not the source of the interface copy — that comes from the
	// message key below.
	Description string
	// DescriptionKey is the message key for the interface description. The
	// server never emits finished copy: it hands over a key and the frontend
	// renders it in the reader's locale.
	DescriptionKey string
	// Group is the admin section; Impact decides whether the write is gated.
	Group  Group
	Impact Impact
	// Section is the integration this key belongs to, e.g. "payments.waffo".
	// Keys of one section are edited and validated together: the interface
	// renders them as one form, and a SectionRule sees the whole section's
	// values at once. Empty means the key stands alone.
	Section string
	// Enum lists the legal values of a KindEnum key.
	Enum []string
	// Range constrains a KindInt, KindMapStringInt or KindMoney key (in nano for
	// money); nil means no constraint.
	Range *Range
	// Default is what is shown before anything is stored, as raw JSON.
	Default json.RawMessage
}

// Registry is the set of keys one assembled server accepts and renders.
//
// The zero value is not usable; build one with NewRegistry. It is safe for
// concurrent reads after assembly, and the mutex is kept because the settings
// page is served while workers read defaults.
type Registry struct {
	mu    sync.RWMutex
	specs map[string]Spec
	// rules are the cross-key invariants, by the section they are declared on.
	rules map[string][]SectionRule
}

// NewRegistry builds a registry from the specs each layer contributes.
//
// It panics on a duplicate key: two layers claiming the same setting is an
// assembly mistake, and the failure it would otherwise produce — one layer's
// description and range silently winning over the other's — is invisible.
func NewRegistry(groups ...[]Spec) *Registry {
	r := &Registry{specs: map[string]Spec{}, rules: map[string][]SectionRule{}}
	for _, group := range groups {
		for _, s := range group {
			if _, dup := r.specs[s.Key]; dup {
				panic("settings: duplicate registration of key " + s.Key)
			}
			r.specs[s.Key] = s
		}
	}
	return r
}

// Lookup returns a registered spec.
func (r *Registry) Lookup(key string) (Spec, bool) {
	if r == nil {
		return Spec{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.specs[key]
	return s, ok
}

// All returns every spec, sorted by key so the admin list is stable.
func (r *Registry) All() []Spec {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Spec, 0, len(r.specs))
	for _, s := range r.specs {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// HasSecrets reports whether any registered key is a KindSecret, which is what
// decides whether a Store needs a Box.
func (r *Registry) HasSecrets() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.specs {
		if s.Kind == KindSecret {
			return true
		}
	}
	return false
}

// Validate checks a JSON value against the registered kind before it is stored.
func (s Spec) Validate(raw json.RawMessage) error {
	switch s.Kind {
	case KindString:
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return errors.New("must be a string")
		}
	case KindEnum:
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return errors.New("must be a string")
		}
		for _, allowed := range s.Enum {
			if v == allowed {
				return nil
			}
		}
		return fmt.Errorf("must be one of %v", s.Enum)
	case KindStringList:
		var v []string
		if err := json.Unmarshal(raw, &v); err != nil {
			return errors.New("must be an array of strings")
		}
	case KindEmail:
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return errors.New("must be a string")
		}
		if v == "" {
			return nil // empty means not configured
		}
		if _, err := mail.ParseAddress(v); err != nil {
			return errors.New("must be a valid email address")
		}
	case KindBool:
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return errors.New("must be a boolean")
		}
	case KindInt:
		var v int64
		if err := json.Unmarshal(raw, &v); err != nil {
			return errors.New("must be an integer")
		}
		if r := s.Range; r != nil {
			if v < r.Min || v > r.Max {
				return fmt.Errorf("must be within [%d, %d]", r.Min, r.Max)
			}
		}
	case KindDecimalString:
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return errors.New("must be a decimal number as a string")
		}
		if !money.IsSignedDecimal(v) {
			return errors.New(`must be a decimal number as a string, e.g. "7.15"`)
		}
	case KindMoney:
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return errors.New("must be an amount as a decimal string")
		}
		if v == "" {
			return nil // empty means not configured
		}
		n, err := money.ParseDecimalNano(v)
		if err != nil {
			return errors.New(`must be an amount as a decimal string with at most nine decimals, e.g. "10.00"`)
		}
		if n < 0 {
			return errors.New("must not be negative")
		}
		if r := s.Range; r != nil && (n < r.Min || n > r.Max) {
			return fmt.Errorf("must be within [%s, %s]", money.FormatNanoExact(r.Min), money.FormatNanoExact(r.Max))
		}
	case KindSecret:
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return errors.New("must be a string")
		}
	case KindMapStringInt:
		var v map[string]int64
		if err := json.Unmarshal(raw, &v); err != nil {
			return errors.New("must be a string-to-integer mapping")
		}
		if r := s.Range; r != nil {
			for k, n := range v {
				if n < r.Min || n > r.Max {
					return fmt.Errorf("the value of %q must be within [%d, %d]", k, r.Min, r.Max)
				}
			}
		}
	default:
		return fmt.Errorf("unknown kind %s", s.Kind)
	}
	return nil
}

// DefaultRetentionDays returns the registered default for a retention key. It is
// exported so callers without a Store — tests, and degraded paths — have
// something to fall back to.
func (r *Registry) DefaultRetentionDays(key string) int { return r.defaultInt(key) }

// defaultInt returns the registered default of a KindInt key, or 0 when the key
// is not registered or is not an integer. Store.RetentionDays falls back to it
// when the setting cannot be read.
func (r *Registry) defaultInt(key string) int {
	sp, ok := r.Lookup(key)
	if !ok {
		return 0
	}
	var v int
	if json.Unmarshal(sp.Default, &v) != nil {
		return 0
	}
	return v
}
