package settings

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

	"github.com/fairlb/fairlb/foundation/money"
)

// A section is an integration: the keys an operator configures as one thing —
// a payment channel's credentials and product, a sign-in provider's client id
// and secret. Two things follow from "as one thing":
//
//   - the keys are written together, in one transaction, so that a half-written
//     channel is never observable; and
//   - the invariants between them ("all or nothing", "the selling channel must
//     name a configured one") are checked against the section's merged values
//     at write time, by a SectionRule.
//
// The rules used to live in the ENV loader, where they ran once at boot and
// where a violation meant the process did not start. Moved here they run on
// every write, the violation is a 422 naming the section, and the same
// function answers `settings check` for an operator and the boot-time warning
// for a deployment whose settings are still empty.

// Values is the effective value of every key a rule can see: the registered
// default, overlaid with what is stored, overlaid with what is being written.
// Secrets appear as their plaintext JSON string. A key that is unset and has no
// default is absent.
type Values map[string]json.RawMessage

// Has reports whether the key carries a value at all.
func (v Values) Has(key string) bool { _, ok := v[key]; return ok }

// String returns the key as a string; a missing key or a non-string is "".
func (v Values) String(key string) string {
	var s string
	if raw, ok := v[key]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

// Int64 returns the key as an integer; a missing key or a non-integer is 0.
func (v Values) Int64(key string) int64 {
	var n int64
	if raw, ok := v[key]; ok {
		_ = json.Unmarshal(raw, &n)
	}
	return n
}

// Nano returns a KindMoney key in nano; a missing key, an empty string or a
// value that does not parse is 0. The registry has already validated what is
// stored, so 0 here means "not configured", which is what every consumer of an
// amount treats it as.
func (v Values) Nano(key string) int64 {
	s := v.String(key)
	if s == "" {
		return 0
	}
	n, err := money.ParseDecimalNano(s)
	if err != nil {
		return 0
	}
	return n
}

// Bool returns the key as a boolean; a missing key or a non-boolean is false.
func (v Values) Bool(key string) bool {
	var b bool
	if raw, ok := v[key]; ok {
		_ = json.Unmarshal(raw, &b)
	}
	return b
}

// Set reports whether the key holds a non-empty value: a non-empty string, a
// non-zero number, or any boolean. It is the "is this field filled in" reading
// that all-or-nothing rules want, where "" and 0 are the shape of "not
// configured" rather than values in their own right.
func (v Values) Set(key string) bool {
	raw, ok := v[key]
	if !ok {
		return false
	}
	switch string(raw) {
	case `""`, `0`, `null`:
		return false
	}
	return true
}

// SectionRule is one invariant over a section's values.
//
// Reads names the other sections the check also looks at: a rule on
// "payments" that asks whether the selling channel is configured reads
// every "payments.<channel>" section. Declaring them does two things — those
// sections' values are loaded and locked alongside the rule's own, and a write
// to any of them re-runs this rule. Without the second, clearing a channel's
// credentials would leave it selected as the selling channel.
type SectionRule struct {
	Section string
	Reads   []string
	Check   func(v Values) error
}

// SectionError is a rule violation. It names the section so the interface can
// attach the message to the right form, and the error text is the rule's own.
type SectionError struct {
	Section string
	Err     error
}

func (e *SectionError) Error() string { return e.Section + ": " + e.Err.Error() }
func (e *SectionError) Unwrap() error { return e.Err }

// MustAddRules registers rules at assembly, right after NewRegistry. Each rule
// must name a section that at least one registered key belongs to, and so must
// every section it reads: a rule on a section nobody registered would never
// run, and the mistake would look exactly like the rule passing.
func (r *Registry) MustAddRules(rules ...SectionRule) {
	r.mu.Lock()
	defer r.mu.Unlock()
	known := map[string]bool{}
	for _, s := range r.specs {
		if s.Section != "" {
			known[s.Section] = true
		}
	}
	for _, rule := range rules {
		if rule.Check == nil {
			panic("settings: rule on section " + rule.Section + " has no check")
		}
		if !known[rule.Section] {
			panic("settings: rule on unknown section " + rule.Section)
		}
		for _, read := range rule.Reads {
			if !known[read] {
				panic("settings: rule on " + rule.Section + " reads unknown section " + read)
			}
		}
		r.rules[rule.Section] = append(r.rules[rule.Section], rule)
	}
}

// SectionKeys returns the specs of one section, sorted by key.
func (r *Registry) SectionKeys(section string) []Spec {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Spec
	for _, s := range r.specs {
		if s.Section == section {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b Spec) int { return cmp.Compare(a.Key, b.Key) })
	return out
}

// Sections returns every section that has at least one rule, sorted.
func (r *Registry) Sections() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.rules))
	for s := range r.rules {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// rulesTouching returns the rules a write to these sections must re-run — the
// rules declared on them, and the rules of other sections that read them —
// together with the union of sections those rules look at.
func (r *Registry) rulesTouching(sections map[string]bool) (rules []SectionRule, load []string) {
	if r == nil {
		return nil, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	loadSet := map[string]bool{}
	for s := range sections {
		loadSet[s] = true
	}
	for _, group := range r.rules {
		for _, rule := range group {
			hit := sections[rule.Section]
			for _, read := range rule.Reads {
				hit = hit || sections[read]
			}
			if !hit {
				continue
			}
			rules = append(rules, rule)
			loadSet[rule.Section] = true
			for _, read := range rule.Reads {
				loadSet[read] = true
			}
		}
	}
	for s := range loadSet {
		load = append(load, s)
	}
	slices.Sort(load)
	// A section's own rule runs before the rules that read it: "this channel's
	// private key does not parse" is the error to show before "the selling
	// channel is not configured", which is only its consequence.
	slices.SortStableFunc(rules, func(a, b SectionRule) int {
		// 布尔键没有 cmp.Compare，显式两支：没有 Reads 的排前面。
		if (len(a.Reads) == 0) != (len(b.Reads) == 0) {
			if len(a.Reads) == 0 {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.Section, b.Section)
	})
	return rules, load
}

// RequireAllOrNone is the rule shape every integration has: if any of the
// named keys is set, all of them must be. names is the human label per key
// used in the message, so the operator reads "Waffo needs merchant id, private
// key, mode, product id and amount" rather than a list of dotted paths.
func RequireAllOrNone(keys ...string) func(Values) error {
	return func(v Values) error {
		var set, missing []string
		for _, k := range keys {
			if v.Set(k) {
				set = append(set, k)
			} else {
				missing = append(missing, k)
			}
		}
		if len(set) == 0 || len(missing) == 0 {
			return nil
		}
		return fmt.Errorf("setting %s requires %s as well", quoteAll(set), quoteAll(missing))
	}
}

func quoteAll(keys []string) string {
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += ", "
		}
		out += strconv.Quote(k)
	}
	return out
}
