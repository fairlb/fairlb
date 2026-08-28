// Package video is the video job plane's request vocabulary.
//
// # Why this package is pure
//
// It imports catalog and nothing else: no HTTP, no database, no credentials.
// Vendor mappers are the largest and most error-prone body of new code on this
// plane, and keeping them pure functions over JSON is what makes each one
// testable from a recorded request/response pair with no fixture database.
//
// It is also the only mechanism that holds ADR-0219's fence in place. A mapper
// that could reach a connection pool would eventually issue a request from
// inside itself, and "the shape is decided here, the delivery happens in proxy"
// would stop being true.
//
// # Why parameters are normalised here at all
//
// The inference data plane never translates: an OpenAI-shaped request reaches
// an OpenAI-shaped upstream. Video has no such protocol to keep -- no vendor
// publishes one the others speak -- so this gateway publishes its own contract
// and maps it per vendor (ADR-0218). The bound on that exception is that the
// normalised set is closed and enumerated below, and anything outside it is
// refused rather than approximated.
package video

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Request is the normalised vocabulary of the video plane.
//
// The list is closed, and that being true is the premise of ADR-0218's argument
// that normalising video parameters is safe. It was chosen against the
// published contracts of Veo 3.1, Kling and Seedance rather than from a guess
// at what they have in common, and two things that survey settled:
//
//   - the *values* have almost nothing in common. Duration is 4/6/8 on Veo,
//     5/10 on Kling and 4/8/12 on Seedance; aspect ratio is two values on Veo
//     and seven on Seedance; Kling has no resolution field at all. That is why
//     the admissible set is declared per route and refused rather than clamped
//     -- there is no value here that "roughly" means the same thing everywhere.
//   - the *fields* mostly do. Every one below appears, under some spelling, in
//     at least two of the three.
//
// A field that belongs to exactly one upstream is not here and has no escape
// hatch here either. Kling's camera control, Veo's personGeneration and
// Seedance's fixed lens are reached on that vendor's own compatibility surface,
// where they are first-class rather than wrapped -- see native.go. This set is
// closed, and "closed" is the premise ADR-0218's safety argument rests on.
type Request struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	// NegativePrompt lists what to keep out of the clip. Near-universal --
	// Kling, Wan, Runway and Luma take it, and Vertex accepts it for Veo -- and
	// it is a plain string everywhere, so it normalises without a decision.
	NegativePrompt string `json:"negative_prompt"`
	// DurationSeconds is the requested length of the clip, and the primary
	// billing quantity. It is an admitted parameter rather than something read
	// back afterwards, which is what makes the charge knowable before the
	// upstream is called at all (ADR-0220).
	DurationSeconds int `json:"duration_seconds"`
	// Resolution is the quality tier, spelled as a resolution: "480p", "720p",
	// "1080p", "4k".
	//
	// Spelled that way because it is what the caller actually wants to choose
	// and what two of the three vendors call it. Kling is the exception and it
	// is worth naming: it has no resolution field, it has `mode` (std / pro /
	// 4k), and its mapper turns a requested resolution into the mode that
	// produces it. That is a real mapping, not a fudge -- pro *is* how Kling
	// delivers 1080p -- but it is a mapping, so it lives in one vendor file
	// with a comment rather than being implied by a shared field name.
	Resolution string `json:"resolution"`
	// AspectRatio is "16:9", "9:16", "1:1" and, on some models, more.
	AspectRatio string `json:"aspect_ratio"`
	// Audio is tri-state on the wire: absent means "whatever this model does by
	// default". The three vendors genuinely differ -- Veo 3.1 generates audio
	// natively and takes no parameter for it, Seedance has a boolean defaulting
	// to off, Kling has none -- so the flag is resolved against the envelope
	// before pricing, because the rate can differ between a silent clip and a
	// scored one.
	Audio *bool `json:"audio"`
	// Image is the first frame to animate from.
	Image string `json:"image"`
	// LastFrame is the frame to interpolate towards. A separate field from
	// Image rather than a second element of one list, because the two are
	// separate fields upstream too (`lastFrame` on Veo, `image_tail` on Kling)
	// and are separately gated: Kling accepts an end frame only in pro mode.
	// A single list would leave the mapper guessing which entry meant which,
	// and this plane refuses rather than guesses.
	LastFrame string `json:"last_frame"`
	// ReferenceImages steer the subject or style without being frames of the
	// output. Typed for the same reason they are separate from Image: Veo takes
	// up to three and asks what each one is for, and a bare URL cannot answer.
	ReferenceImages []ReferenceImage `json:"reference_images"`
	Seed            *int64           `json:"seed"`
	// N is how many clips to produce. Absent means one.
	N int `json:"n"`
	// Passthrough carries parameters that belong to exactly one upstream, and
	// only a compatibility surface ever fills it.
	//
	// `json:"-"` is load-bearing twice over: a caller on this plane cannot set
	// it (Decode refuses the field name like any other it does not know), and a
	// request cannot carry it back out. On a vendor's own surface these are
	// first-class parameters the caller wrote in that vendor's own vocabulary,
	// and the surface has already read every one that changes a billed quantity
	// into the fields above -- what is left has no effect on price and is
	// forwarded as written.
	//
	// This is not the escape hatch that used to live here. That one let a
	// caller on the normalised plane name a vendor and smuggle its parameters
	// through; the argument for it was that a contract unable to express a
	// headline feature sends the caller to the vendor directly. A
	// compatibility surface answers that better, and is where they now go.
	Passthrough map[string]json.RawMessage `json:"-"`
}

// ReferenceImage is one steering image and what it is for.
type ReferenceImage struct {
	// URL is an https or data URL.
	URL string `json:"url"`
	// Type is "asset" (keep this subject) or "style" (look like this).
	Type string `json:"type"`
}

// Reference image types.
const (
	ReferenceAsset = "asset"
	ReferenceStyle = "style"
)

// ErrUnknownParameter is returned when a request names a field outside the
// normalised set.
//
// Refusing is the point rather than a strictness preference. A gateway that
// silently drops an unrecognised parameter produces a clip the caller did not
// ask for and bills them for it, and the caller has no way to find out. On a
// plane where every request costs real money and the parameter space is small
// enough to enumerate, "I do not know what you meant" must be an answer.
type ErrUnknownParameter struct{ Field string }

func (e ErrUnknownParameter) Error() string {
	return fmt.Sprintf("unknown parameter %q; the video API accepts model, prompt, "+
		"negative_prompt, duration_seconds, resolution, aspect_ratio, audio, image, "+
		"last_frame, reference_images, seed and n. A parameter that belongs to one "+
		"upstream alone is reached on that vendor's own compatibility surface", e.Field)
}

// Decode parses a request body, refusing anything outside the normalised set.
func Decode(body []byte) (Request, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	dec.UseNumber()

	var r Request
	if err := dec.Decode(&r); err != nil {
		if field, ok := unknownField(err); ok {
			return Request{}, ErrUnknownParameter{Field: field}
		}
		return Request{}, fmt.Errorf("invalid request body: %w", err)
	}
	// A second document after the first is a malformed body, not a batch.
	if dec.More() {
		return Request{}, fmt.Errorf("invalid request body: unexpected trailing content")
	}
	if r.N == 0 {
		r.N = 1
	}
	r.Model = strings.TrimSpace(r.Model)
	r.Resolution = strings.TrimSpace(r.Resolution)
	r.AspectRatio = strings.TrimSpace(r.AspectRatio)
	return r, nil
}

// unknownField recovers the offending field name from encoding/json's error,
// which reports it only inside a message string.
func unknownField(err error) (string, bool) {
	const prefix = `json: unknown field "`
	msg := err.Error()
	i := strings.Index(msg, prefix)
	if i < 0 {
		return "", false
	}
	rest := msg[i+len(prefix):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}
