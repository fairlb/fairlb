package gwstaffapi_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/foundation/httpx"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
)

// A slug cannot be changed once it exists, model lookup is an exact match with
// no fallback, and `owned_by` is the segment before the first slash. One bare
// upstream name accepted here is therefore permanent, unreachable under its
// documented name, and creator-less in the public catalog -- which is why the
// refusal lives in the database rather than in a form.
func TestCreatingAModelRefusesAnythingThatIsNotTwoSegments(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()

	refused := []struct {
		slug, why string
	}{
		{"gpt-5.6-sol", "a bare upstream name, which is what the discovery flow used to produce"},
		{"openai/GPT-5.6-Sol", "upper case, so two spellings could name one model"},
		{"openai/gpt-5.6/sol", "three segments"},
		{"openai/", "no name"},
		{"/gpt-5.6-sol", "no creator"},
		{"openai gpt", "a space"},
		{"openai//sol", "an empty middle segment"},
	}
	for _, c := range refused {
		t.Run(c.slug, func(t *testing.T) {
			slug := c.slug
			_, err := s.CreateGatewayModel(ctx, gwstaffapi.CreateGatewayModelRequestObject{
				Body: &gwstaffapi.GatewayModelInput{Slug: &slug},
			})
			if err == nil {
				t.Fatalf("%q was accepted (%s)", c.slug, c.why)
			}
			// Refused as a validation error, not as a 500: the caller can fix
			// this, and a server error would say the opposite.
			var ce *httpx.CodeError
			if !errors.As(err, &ce) || ce.Code != errcode.CommonValidation {
				t.Fatalf("%q refused with %v, want a validation refusal", c.slug, err)
			}
			if !strings.Contains(ce.Detail, "creator") {
				t.Errorf("the message should say what a slug looks like, got %q", ce.Detail)
			}
		})
	}

	// And the shape it exists to admit.
	for _, slug := range []string{
		"openai/gpt-5.6-sol", "anthropic/claude-haiku-4-5",
		"google/gemini-3.1-pro-preview", "x-ai/grok-4", "openai/gpt-image-2",
	} {
		if m := mustModel(t, s, slug); m.Slug != slug {
			t.Errorf("stored %q, want %q", m.Slug, slug)
		}
	}
}

// The Go mirror of the constraint and the constraint itself must agree.
//
// Discovery uses the mirror to decide whether a name is worth suggesting, so a
// mirror that is looser offers prefills that fail on save, and one that is
// stricter silently withholds suggestions that would have worked. Neither
// shows up as an error anywhere -- hence this.
func TestModelSlugMirrorAgreesWithTheDatabase(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()

	for _, slug := range []string{
		"openai/gpt-5.6-sol", "openai/gpt-5.4", "openai/gpt-image-2",
		"anthropic/claude-opus-4-8", "google/gemini-3.1-pro-preview",
		"x-ai/grok-4", "z-ai/glm-4.6", "moonshotai/kimi-k2", "qwen/qwen3-max",
		"a/b", "a-b/c-d", "a1/b2.c3", "a/b_c",
		"gpt-5.6-sol", "openai/GPT", "OPENAI/gpt", "openai//x", "openai/",
		"/x", "", "openai/gpt 5", "openai/gpt/5", "-openai/gpt", "openai/-gpt",
		"openai/gpt-", "openai/.gpt", "openai/gpt.",
	} {
		mirror := catalog.ValidModelSlug(slug)
		s2 := slug
		_, err := s.CreateGatewayModel(ctx, gwstaffapi.CreateGatewayModelRequestObject{
			Body: &gwstaffapi.GatewayModelInput{Slug: &s2},
		})
		accepted := err == nil
		if mirror != accepted {
			t.Errorf("%q: the mirror says valid=%v, the database says accepted=%v (%v)",
				slug, mirror, accepted, err)
		}
	}
}
