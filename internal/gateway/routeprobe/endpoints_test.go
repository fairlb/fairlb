package routeprobe

import "testing"

// Image endpoints are never probed automatically, and neither are the stored-
// resource operations.
//
// This guards a promise already made to the operator: the UI says an image
// probe runs once per click and never on its own, because it spends real money.
// A rule that quietly started probing them would overturn that promise and make
// the cost of a bulk adoption unbounded.
//
// The assertion moved here with the code (ADR-0175). It used to live in the
// staff handler's test package, reaching an exported-for-test wrapper -- which
// is what a rule looks like when it lives in the wrong package.
func TestAutoProbeableExcludesImagesAndStoredResources(t *testing.T) {
	for _, tc := range []struct {
		name string
		all  []string
		want []string
	}{
		{"images and stored resources drop out",
			[]string{"chat", "images", "embeddings", "responses_resources"},
			[]string{"chat", "embeddings"}},
		{"only images leaves nothing to probe", []string{"images"}, nil},
		{"a stored-resource operation has no resource id to probe with",
			[]string{"responses_resources"}, nil},
		{"an empty declaration stays empty", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := AutoProbeable(tc.all)
			if len(got) != len(tc.want) {
				t.Fatalf("AutoProbeable(%v) = %v, want %v", tc.all, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("AutoProbeable(%v) = %v, want %v", tc.all, got, tc.want)
				}
			}
		})
	}
}

// Probeable is the wider filter: it keeps images, because a manual probe of an
// image endpoint is exactly what the operator's button is for. Asserting both
// filters separately is what keeps "excluded from automatic probing" from
// quietly becoming "excluded from probing".
func TestProbeableKeepsImages(t *testing.T) {
	got := Probeable([]string{"chat", "images", "responses_resources"})
	if len(got) != 2 || got[0] != "chat" || got[1] != "images" {
		t.Fatalf("Probeable dropped the wrong endpoints: %v", got)
	}
}
