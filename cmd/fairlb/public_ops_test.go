package main

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The allowlist in the router and the `security: []` markers in the spec are
// two statements of the same fact, so they can disagree. This reconciles them
// in both directions.
//
// Both directions matter, and they fail differently. An operation marked public
// in the spec but missing from the allowlist answers 401 — documented as open,
// closed in practice, which someone reports. An operation in the allowlist but
// not marked public in the spec is open in practice and documented as
// protected, which nobody reports because nothing looks wrong.
func TestPublicStaffOperationsMatchTheSpec(t *testing.T) {
	raw, err := os.ReadFile("../../api/staff.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Paths) == 0 {
		t.Fatal("no paths parsed out of the spec — this test is not reading what it thinks it is")
	}

	// A path item may also carry keys that are not operations (`parameters` is
	// a list, and decoding it as one would fail), so only HTTP methods count.
	methods := map[string]bool{
		"get": true, "post": true, "put": true, "patch": true,
		"delete": true, "head": true, "options": true,
	}

	inSpec := map[string]bool{}
	total := 0
	for path, item := range spec.Paths {
		for key, raw := range item {
			if !methods[key] {
				continue
			}
			total++
			op, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("%s %s is not a mapping", key, path)
			}
			// `security: []` overrides the document-level requirement; an
			// absent key means "inherit", which here means a session.
			sec, present := op["security"]
			if !present {
				continue
			}
			list, ok := sec.([]any)
			if ok && len(list) == 0 {
				inSpec[upper(key)+" "+path] = true
			}
		}
	}
	// A guard against the parse quietly producing nothing useful: this spec has
	// operations, and some of them are public.
	if total == 0 || len(inSpec) == 0 {
		t.Fatalf("parsed %d operations, %d public — the spec shape changed", total, len(inSpec))
	}

	for op := range inSpec {
		if _, ok := publicStaffOps[op]; !ok {
			t.Errorf("%s is `security: []` in the spec but is not in publicStaffOps, "+
				"so it answers 401 — documented as public, closed in practice", op)
		}
	}
	for op, reason := range publicStaffOps {
		if !inSpec[op] {
			t.Errorf("%s is in publicStaffOps (%q) but is not `security: []` in the spec, "+
				"so it is open in practice and documented as protected", op, reason)
		}
		if reason == "" {
			t.Errorf("%s has no reason recorded", op)
		}
	}
}

// The middleware matches on full paths, so the expansion has to include the
// mount prefix. Getting this wrong makes every entry silently ineffective: the
// endpoints stay protected and setup answers 401 with no account able to exist.
func TestAnonymousStaffPathsCarryTheMountPrefix(t *testing.T) {
	paths := anonymousStaffPaths()
	if len(paths) != len(publicStaffOps) {
		t.Fatalf("expanded %d paths from %d operations", len(paths), len(publicStaffOps))
	}
	for p := range paths {
		if !strings.Contains(p, staffAPIPrefix+"/") {
			t.Errorf("%q does not carry the mount prefix %q", p, staffAPIPrefix)
		}
	}
	if !paths["POST "+staffAPIPrefix+"/setup"] {
		t.Error("setup is not reachable anonymously; nobody could create the first administrator")
	}
}

func upper(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'a' && c <= 'z' {
			out[i] = c - 32
		}
	}
	return string(out)
}
