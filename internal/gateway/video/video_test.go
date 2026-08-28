package video

import (
	"errors"
	"strings"
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

func veo() Envelope {
	return Envelope{
		DurationsSeconds:       []int{4, 6, 8},
		Resolutions:            []string{"720p", "1080p"},
		AspectRatios:           []string{"16:9", "9:16"},
		Audio:                  AudioOptional,
		MaxN:                   1,
		SupportsNegativePrompt: true,
		Cancel:                 CancelNever,
		MaxJobSeconds:          900,
		Source:                 "declared",
	}
}

// The premise of ADR-0218's whole argument: the normalised set is closed, and
// anything outside it is refused rather than dropped.
func TestAnUnknownParameterIsRefusedNotDropped(t *testing.T) {
	_, err := Decode([]byte(`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8,"style":"anime"}`))
	var unknown ErrUnknownParameter
	if !errors.As(err, &unknown) {
		t.Fatalf("an unrecognised parameter must be refused, got %v", err)
	}
	if unknown.Field != "style" {
		t.Fatalf("the refusal must name the field, got %q", unknown.Field)
	}
	if !strings.Contains(unknown.Error(), "duration_seconds") {
		t.Errorf("the message should list what is accepted, got %q", unknown.Error())
	}
}

func TestDecodeDefaultsNToOne(t *testing.T) {
	r, err := Decode([]byte(`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.N != 1 {
		t.Fatalf("n defaulted to %d", r.N)
	}
	if r.Audio != nil {
		t.Fatal("an absent audio flag must stay absent, not become false")
	}
}

// The single most important refusal on this plane. A shortened clip billed at
// the requested length is exactly the substitution the envelope exists to
// prevent, and it is what would make normalising parameters unsafe.
func TestAnOverlongRequestIsRefusedNotShortened(t *testing.T) {
	r := Request{Model: "google/veo-3.1", Prompt: "a cat", DurationSeconds: 12, N: 1}
	err := veo().Validate(r, false)
	var outside ErrOutsideEnvelope
	if !errors.As(err, &outside) {
		t.Fatalf("12s against a 4/6/8s model must be refused, got %v", err)
	}
	if outside.Field != "duration_seconds" {
		t.Fatalf("refusal named %q", outside.Field)
	}
	if !strings.Contains(outside.Error(), "4, 6, 8") {
		t.Errorf("the caller must be told what is admissible, got %q", outside.Error())
	}
}

func TestEnvelopeRefusalsCoverEveryNormalisedAxis(t *testing.T) {
	base := Request{Model: "google/veo-3.1", Prompt: "a cat", DurationSeconds: 8,
		Resolution: "720p", AspectRatio: "16:9", N: 1}
	e := veo()
	if err := e.Validate(base, false); err != nil {
		t.Fatalf("the baseline request must be admissible: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(Request) Request
		audio  bool
		field  string
	}{
		{"resolution", func(r Request) Request { r.Resolution = "4k"; return r }, false, "resolution"},
		{"aspect ratio", func(r Request) Request { r.AspectRatio = "21:9"; return r }, false, "aspect_ratio"},
		{"n", func(r Request) Request { r.N = 4; return r }, false, "n"},
		{"image to video", func(r Request) Request { r.Image = "data:,x"; return r }, false, "image"},
		{"empty prompt", func(r Request) Request { r.Prompt = ""; return r }, false, "prompt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var outside ErrOutsideEnvelope
			if err := e.Validate(tc.mutate(base), tc.audio); !errors.As(err, &outside) {
				t.Fatalf("expected a refusal, got %v", err)
			} else if outside.Field != tc.field {
				t.Fatalf("refusal named %q, want %q", outside.Field, tc.field)
			}
		})
	}
}

func TestAudioIsRefusedOnASilentModelRatherThanIgnored(t *testing.T) {
	silent := veo()
	silent.Audio = AudioNever
	r := Request{Model: "m", Prompt: "a cat", DurationSeconds: 8, N: 1}
	if err := silent.Validate(r, true); err == nil {
		t.Fatal("asking a silent model for audio must be refused, not quietly served without sound")
	}
	if err := silent.Validate(r, false); err != nil {
		t.Fatalf("the same model without audio must be fine: %v", err)
	}
}

// An operator who has not said whether a model can be stopped has not promised
// that it can.
func TestUnsetCancelReadsAsNever(t *testing.T) {
	var e Envelope
	if got := e.CancelModeOrDefault(); got != CancelNever {
		t.Fatalf("an unset cancel mode read as %q", got)
	}
}

// The union is what admission validates against, because the hold is taken
// before a route is chosen.
func TestUnionAdmitsWhatAnyRouteCanServeAndCoversNarrowsAgain(t *testing.T) {
	short := Envelope{DurationsSeconds: []int{4}, Resolutions: []string{"720p"},
		AspectRatios: []string{"16:9"}, Audio: AudioNever, Cancel: CancelNever}
	long := Envelope{DurationsSeconds: []int{8, 12}, Resolutions: []string{"1080p"},
		AspectRatios: []string{"16:9"}, Audio: AudioAlways, Cancel: CancelAnytime}

	u := Union([]Envelope{short, long})
	if want := []int{4, 8, 12}; len(u.DurationsSeconds) != len(want) {
		t.Fatalf("union durations %v", u.DurationsSeconds)
	}
	if u.Audio != AudioOptional {
		t.Fatalf("one silent and one scored model make audio optional overall, got %q", u.Audio)
	}
	if u.Cancel != CancelAnytime {
		t.Fatalf("the union reports the most capable cancel any route offers, got %q", u.Cancel)
	}

	twelve := Request{Model: "m", Prompt: "a cat", DurationSeconds: 12, Resolution: "1080p", AspectRatio: "16:9", N: 1}
	if err := u.Validate(twelve, true); err != nil {
		t.Fatalf("the union must admit what some route can serve: %v", err)
	}
	if short.Covers(twelve, true) {
		t.Fatal("the narrow route must be filtered out of the candidates")
	}
	if !long.Covers(twelve, true) {
		t.Fatal("the capable route must survive candidate filtering")
	}
}

// The charge is a pure function of admitted parameters.
func TestBillingUnitsMultipliesSecondsByN(t *testing.T) {
	r := Request{DurationSeconds: 8, Resolution: "1080p", N: 3}
	u := BillingUnits(r, true, catalog.UnitSecond)
	want := catalog.UnitKey{Unit: catalog.UnitSecond, Resolution: "1080p", Audio: "on"}
	if got := u.Quantities[want]; got != 24 {
		t.Fatalf("3 clips of 8s billed %d seconds, want 24 (quantities: %v)", got, u.Quantities)
	}

	perCall := BillingUnits(r, false, catalog.UnitCall)
	callKey := catalog.UnitKey{Unit: catalog.UnitCall, Resolution: "1080p", Audio: "off"}
	if got := perCall.Quantities[callKey]; got != 3 {
		t.Fatalf("a per-call model billed %d calls for n=3", got)
	}
}

// The whole admission-pricing chain in one place, because the claim it makes is
// the plane's headline: what a job costs is knowable before it starts.
func TestAdmittedRequestPricesExactlyAndRefusedOneNeverPrices(t *testing.T) {
	e := veo()
	rates := catalog.NewUnitPriceTable([]gwdb.ModelPriceUnitRate{
		{Unit: "second", Resolution: "720p", Audio: "on", ServiceTier: catalog.TierStandard, NanoPerUnit: 400_000_000},
	})
	r := catalog.Rates{FXRate: "1"}

	req, err := Decode([]byte(`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":8,"resolution":"720p","aspect_ratio":"16:9","audio":true}`))
	if err != nil {
		t.Fatal(err)
	}
	audioOn := e.ResolveAudio(req)
	if err := e.Validate(req, audioOn); err != nil {
		t.Fatalf("an in-envelope request must be admitted: %v", err)
	}
	q, err := catalog.ComputeUnits(rates, rates, BillingUnits(req, audioOn, catalog.UnitSecond), r)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(3_200_000_000); q.ChargedNano != want {
		t.Fatalf("8s at $0.40/s = %d nano, got %d", want, q.ChargedNano)
	}

	// And the refusal happens before anything is priced at all.
	over, err := Decode([]byte(`{"model":"google/veo-3.1","prompt":"a cat","duration_seconds":12,"resolution":"720p","audio":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Validate(over, e.ResolveAudio(over)); err == nil {
		t.Fatal("a 12s request against a 4/6/8s model reached pricing")
	}
}

// The three image roles are distinct upstream and are gated separately. A model
// that animates a first frame does not necessarily interpolate to a last one.
func TestImageRolesAreGatedSeparately(t *testing.T) {
	e := veo()
	e.SupportsImageToVideo = true
	e.SupportsLastFrame = false
	e.MaxReferenceImages = 3

	base := Request{Model: "m", Prompt: "a cat", DurationSeconds: 8, N: 1}

	withStart := base
	withStart.Image = "https://example.test/a.png"
	if err := e.Validate(withStart, false); err != nil {
		t.Fatalf("a first frame must be accepted by an image-to-video model: %v", err)
	}

	withEnd := base
	withEnd.LastFrame = "https://example.test/b.png"
	var outside ErrOutsideEnvelope
	if err := e.Validate(withEnd, false); !errors.As(err, &outside) || outside.Field != "last_frame" {
		t.Fatalf("an end frame must be refused where interpolation is not supported, got %v", err)
	}

	tooMany := base
	for range 4 {
		tooMany.ReferenceImages = append(tooMany.ReferenceImages,
			ReferenceImage{URL: "https://example.test/r.png", Type: ReferenceAsset})
	}
	if err := e.Validate(tooMany, false); !errors.As(err, &outside) || outside.Field != "reference_images" {
		t.Fatalf("four reference images against a limit of three must be refused, got %v", err)
	}

	untyped := base
	untyped.ReferenceImages = []ReferenceImage{{URL: "https://example.test/r.png"}}
	if err := e.Validate(untyped, false); !errors.As(err, &outside) {
		t.Fatalf("a reference image with no stated role must be refused rather than guessed at, got %v", err)
	}
}

// Dropping a negative prompt silently would let a caller pay for a clip that
// ignored half of what they asked for.
func TestNegativePromptIsRefusedWhereUnsupported(t *testing.T) {
	e := veo()
	e.SupportsNegativePrompt = false
	r := Request{Model: "m", Prompt: "a cat", NegativePrompt: "blurry", DurationSeconds: 8, N: 1}
	var outside ErrOutsideEnvelope
	if err := e.Validate(r, false); !errors.As(err, &outside) || outside.Field != "negative_prompt" {
		t.Fatalf("an unsupported negative prompt must be refused, not dropped, got %v", err)
	}
}

// The survey finding that justifies declaring the admissible set per route:
// the three vendors' duration values barely overlap, so there is no value that
// "roughly" means the same thing everywhere.
func TestRealVendorDurationSetsDoNotOverlap(t *testing.T) {
	veoE := Envelope{DurationsSeconds: []int{4, 6, 8}, Resolutions: []string{"720p", "1080p", "4k"},
		AspectRatios: []string{"16:9", "9:16"}, Audio: AudioAlways, MaxN: 1}
	klingE := Envelope{DurationsSeconds: []int{5, 10}, Resolutions: []string{"720p", "1080p"},
		AspectRatios: []string{"16:9", "9:16", "1:1"}, Audio: AudioNever, MaxN: 1}

	five := Request{Model: "m", Prompt: "a cat", DurationSeconds: 5, N: 1}
	if err := veoE.Validate(five, true); err == nil {
		t.Error("5s is a Kling duration and must not be accepted by a 4/6/8 model")
	}
	if err := klingE.Validate(five, false); err != nil {
		t.Errorf("5s must be accepted by Kling: %v", err)
	}

	// And the union admits both, which is what the catalog publishes.
	u := Union([]Envelope{veoE, klingE})
	for _, d := range []int{4, 5, 6, 8, 10} {
		r := Request{Model: "m", Prompt: "a cat", DurationSeconds: d, N: 1}
		if err := u.Validate(r, false); err != nil {
			t.Errorf("the union must admit %ds, which some route serves: %v", d, err)
		}
	}
}

// Duration is the billing quantity, so it is bounded unconditionally -- not
// only when the envelope happens to enumerate durations. A route declaring
// resolutions but no durations used to admit duration_seconds:0, which
// multiplies out to a quantity of zero and bills a generated video at nothing.
func TestZeroDurationIsRefusedEvenWithNoDeclaredDurations(t *testing.T) {
	e := Envelope{Resolutions: []string{"720p"}, AspectRatios: []string{"16:9"},
		Audio: AudioNever, MaxN: 1}
	for _, seconds := range []int{0, -5} {
		r := Request{Model: "m", Prompt: "a cat", DurationSeconds: seconds, Resolution: "720p", N: 1}
		var outside ErrOutsideEnvelope
		if err := e.Validate(r, false); !errors.As(err, &outside) || outside.Field != "duration_seconds" {
			t.Fatalf("duration_seconds=%d must be refused, got %v", seconds, err)
		}
	}
}

// The normalised set is closed, and closed is the premise ADR-0218's argument
// rests on: a parameter this gateway does not recognise is refused rather than
// dropped, because a dropped parameter produces a clip the caller did not ask
// for and is billed for anyway.
//
// vendor_options used to be the one door in this wall, and the rule that kept
// it honest was "an option may not shadow a priced parameter". The door is gone
// -- a knob belonging to one upstream is reached on that vendor's own
// compatibility surface -- so what is left to assert is the wall itself. The
// shadowing rule has a successor there, where the vendor's own spelling of a
// priced quantity has to be read rather than forwarded.
func TestTheNormalisedSetIsClosed(t *testing.T) {
	for _, field := range []string{
		"vendor_options", "camera_control", "cfg_scale", "personGeneration",
		"fixed_lens", "watermark", "prompt_optimizer",
	} {
		body := []byte(`{"model":"kuaishou/kling-v2","prompt":"a cat","duration_seconds":5,"` +
			field + `":true}`)
		var unknown ErrUnknownParameter
		if _, err := Decode(body); !errors.As(err, &unknown) {
			t.Errorf("%q was not refused; an unrecognised parameter must never be dropped silently, got %v",
				field, err)
		} else if unknown.Field != field {
			t.Errorf("the refusal named %q rather than the offending %q", unknown.Field, field)
		}
	}
}
