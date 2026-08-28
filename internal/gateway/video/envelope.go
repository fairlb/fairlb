package video

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// CancelMode is how far a model can be stopped once a job exists.
//
// It is a declared capability rather than a universal verb because the vendors
// genuinely differ: one publishes a real cancel with its own terminal state,
// another accepts cancel only while the job is still queued, a third documents
// none at all. Publishing one "cancel" that silently does nothing on two thirds
// of the catalog is the lie ADR-0157 forbids, so the difference is stated and
// GET /v1/videos/models reports it (ADR-0221).
type CancelMode string

const (
	CancelNever      CancelMode = "never"
	CancelQueuedOnly CancelMode = "queued_only"
	CancelAnytime    CancelMode = "anytime"
)

// AudioSupport is what a model can do about sound.
type AudioSupport string

const (
	AudioNever    AudioSupport = "never"
	AudioOptional AudioSupport = "optional"
	AudioAlways   AudioSupport = "always"
)

// Envelope is what one deployment of a video model accepts.
//
// # Why this is declared and not observed
//
// ADR-0209 established that capability is observed, because configuration that
// restates an upstream drifts. This is the one deliberate exception, and the
// reason is that the observation is not affordable: answering "does this model
// accept a twelve-second clip" costs a twelve-second clip, and the value space
// is duration x resolution x aspect ratio x audio.
//
// What makes the exception safe is that neither direction of drift produces a
// wrong bill. Declaring too little refuses the request at admission having
// spent nothing. Declaring too much ends in an upstream rejection, a terminal
// `failed` job, and a voided hold -- the caller pays nothing either way. The
// `endpoints` column ADR-0209 deleted had no such property: over-declaring
// there put live traffic on a route that could never work.
//
// Endpoint reachability is still observed. This says nothing about whether the
// route serves video at all; the probe verdict does.
type Envelope struct {
	DurationsSeconds     []int        `json:"durations_seconds,omitempty"`
	Resolutions          []string     `json:"resolutions,omitempty"`
	AspectRatios         []string     `json:"aspect_ratios,omitempty"`
	Audio                AudioSupport `json:"audio,omitempty"`
	MaxN                 int          `json:"max_n,omitempty"`
	SupportsImageToVideo bool         `json:"supports_image_to_video,omitempty"`
	// SupportsLastFrame is separate from SupportsImageToVideo because upstreams
	// gate the two separately: Kling accepts an end frame only in pro mode, and
	// Veo added frame interpolation after image-to-video.
	SupportsLastFrame bool `json:"supports_last_frame,omitempty"`
	// MaxReferenceImages is 0 where steering images are not accepted at all.
	// Veo takes up to three; most take none.
	MaxReferenceImages int `json:"max_reference_images,omitempty"`
	// SupportsNegativePrompt is declared rather than assumed. It is
	// near-universal but not universal, and silently dropping it would let a
	// caller pay for a clip that ignored half of what they asked for.
	SupportsNegativePrompt bool `json:"supports_negative_prompt,omitempty"`
	// MaxPromptChars is measured in characters, and upstream limits are not
	// always: Seedance publishes 2000 characters, Veo publishes 1024 *tokens*.
	// Configuring a token limit here as if it were characters would refuse
	// valid prompts, so a model whose limit is not expressed in characters
	// leaves this at 0 and lets the upstream be the one to refuse.
	MaxPromptChars int        `json:"max_prompt_chars,omitempty"`
	Cancel         CancelMode `json:"cancel,omitempty"`
	// MaxJobSeconds bounds how long a job of this model may run before the
	// reconciler calls it expired. It sizes the hold's TTL too, which is why it
	// belongs to the model rather than being one global constant: a four-second
	// clip and a long render are an order of magnitude apart.
	MaxJobSeconds int `json:"max_job_seconds,omitempty"`
	// Source is `observed` when this came from a vendor's own capability
	// endpoint and `declared` when an operator typed it. The two are kept apart
	// for the same reason a reference price keeps `source_name` apart from
	// `verified_at`: a page that shows one as the other is claiming a check
	// nobody performed.
	Source string `json:"source,omitempty"`
}

// ParseEnvelope reads a route's stored envelope. An absent or empty object is
// not an error: most routes are not video routes.
func ParseEnvelope(raw []byte) (Envelope, error) {
	var e Envelope
	if len(raw) == 0 {
		return e, nil
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return Envelope{}, fmt.Errorf("video: invalid capability envelope: %w", err)
	}
	return e, nil
}

// Configured reports whether this route says anything about video at all.
func (e Envelope) Configured() bool {
	return len(e.DurationsSeconds) > 0 || len(e.Resolutions) > 0 || len(e.AspectRatios) > 0
}

// CancelModeOrDefault is the conservative reading of an unset cancel field.
//
// Unset means never, not anytime. An operator who has not said whether a model
// can be stopped has not promised that it can, and offering a cancel that does
// nothing is worse than refusing one.
func (e Envelope) CancelModeOrDefault() CancelMode {
	switch e.Cancel {
	case CancelQueuedOnly, CancelAnytime:
		return e.Cancel
	default:
		return CancelNever
	}
}

// Union merges the envelopes of every candidate route into what the model as a
// whole accepts.
//
// This is what GET /v1/videos/models publishes and what admission validates
// against, because the hold is taken before a route is chosen. A request inside
// the union but servable by only one route simply has one candidate; the
// envelope then acts as a candidate filter, the same way capacity and the
// breaker do. Price never varies by route, so narrowing can only change who
// serves the job, never what it costs.
func Union(envelopes []Envelope) Envelope {
	var u Envelope
	for _, e := range envelopes {
		if !e.Configured() {
			continue
		}
		u.DurationsSeconds = mergeInts(u.DurationsSeconds, e.DurationsSeconds)
		u.Resolutions = mergeStrings(u.Resolutions, e.Resolutions)
		u.AspectRatios = mergeStrings(u.AspectRatios, e.AspectRatios)
		u.Audio = widerAudio(u.Audio, e.Audio)
		u.Cancel = widerCancel(u.Cancel, e.CancelModeOrDefault())
		u.SupportsImageToVideo = u.SupportsImageToVideo || e.SupportsImageToVideo
		u.SupportsLastFrame = u.SupportsLastFrame || e.SupportsLastFrame
		u.SupportsNegativePrompt = u.SupportsNegativePrompt || e.SupportsNegativePrompt
		u.MaxReferenceImages = max(u.MaxReferenceImages, e.MaxReferenceImages)
		u.MaxN = max(u.MaxN, e.MaxN)
		u.MaxPromptChars = max(u.MaxPromptChars, e.MaxPromptChars)
		u.MaxJobSeconds = max(u.MaxJobSeconds, e.MaxJobSeconds)
	}
	return u
}

// Covers reports whether this one route can serve an already-admitted request.
// Used to filter candidates after the union has admitted the request.
func (e Envelope) Covers(r Request, audioOn bool) bool {
	return e.Validate(r, audioOn) == nil
}

// ErrOutsideEnvelope is a request the model cannot serve. It carries the
// admissible set so the caller can correct it without guessing.
type ErrOutsideEnvelope struct {
	Field      string
	Got        string
	Admissible []string
}

func (e ErrOutsideEnvelope) Error() string {
	if len(e.Admissible) == 0 {
		return fmt.Sprintf("this model does not support %s=%s", e.Field, e.Got)
	}
	return fmt.Sprintf("this model does not support %s=%s; it accepts %s",
		e.Field, e.Got, strings.Join(e.Admissible, ", "))
}

// Validate refuses a request the envelope does not cover.
//
// Every arm refuses; none clamps. A twelve-second request against a model that
// tops out at eight must be an error, never a silently shortened clip billed
// for twelve -- that substitution is the failure this whole check exists to
// prevent, and it is the premise of ADR-0218's argument that normalising video
// parameters is safe.
func (e Envelope) Validate(r Request, audioOn bool) error {
	if !e.Configured() {
		return ErrOutsideEnvelope{Field: "model", Got: r.Model}
	}
	if r.Prompt == "" && r.Image == "" {
		return ErrOutsideEnvelope{Field: "prompt", Got: "(empty)"}
	}
	if r.NegativePrompt != "" && !e.SupportsNegativePrompt {
		return ErrOutsideEnvelope{Field: "negative_prompt", Got: "(set)"}
	}
	if e.MaxPromptChars > 0 && len(r.Prompt) > e.MaxPromptChars {
		return ErrOutsideEnvelope{Field: "prompt", Got: fmt.Sprintf("%d characters", len(r.Prompt)),
			Admissible: []string{fmt.Sprintf("at most %d", e.MaxPromptChars)}}
	}
	// Checked before the declared-set test, and unconditionally, because
	// duration is the billing quantity. Guarding this only by "if the envelope
	// lists durations" leaves a route that declares resolutions but no
	// durations admitting duration_seconds: 0, which multiplies out to a
	// quantity of zero and bills a generated video at nothing.
	if r.DurationSeconds <= 0 {
		return ErrOutsideEnvelope{Field: "duration_seconds", Got: fmt.Sprint(r.DurationSeconds),
			Admissible: durationAdmissible(e)}
	}
	if len(e.DurationsSeconds) > 0 && !slices.Contains(e.DurationsSeconds, r.DurationSeconds) {
		return ErrOutsideEnvelope{Field: "duration_seconds", Got: fmt.Sprint(r.DurationSeconds),
			Admissible: intsToStrings(e.DurationsSeconds)}
	}
	if r.Resolution != "" && len(e.Resolutions) > 0 && !slices.Contains(e.Resolutions, r.Resolution) {
		return ErrOutsideEnvelope{Field: "resolution", Got: r.Resolution, Admissible: e.Resolutions}
	}
	if r.AspectRatio != "" && len(e.AspectRatios) > 0 && !slices.Contains(e.AspectRatios, r.AspectRatio) {
		return ErrOutsideEnvelope{Field: "aspect_ratio", Got: r.AspectRatio, Admissible: e.AspectRatios}
	}
	if audioOn && e.Audio == AudioNever {
		return ErrOutsideEnvelope{Field: "audio", Got: "true", Admissible: []string{"false"}}
	}
	if !audioOn && e.Audio == AudioAlways {
		return ErrOutsideEnvelope{Field: "audio", Got: "false", Admissible: []string{"true"}}
	}
	if r.Image != "" && !e.SupportsImageToVideo {
		return ErrOutsideEnvelope{Field: "image", Got: "(set)"}
	}
	if r.LastFrame != "" && !e.SupportsLastFrame {
		return ErrOutsideEnvelope{Field: "last_frame", Got: "(set)"}
	}
	if len(r.ReferenceImages) > e.MaxReferenceImages {
		return ErrOutsideEnvelope{Field: "reference_images",
			Got:        fmt.Sprintf("%d", len(r.ReferenceImages)),
			Admissible: []string{fmt.Sprintf("at most %d", e.MaxReferenceImages)}}
	}
	for _, ref := range r.ReferenceImages {
		if ref.Type != ReferenceAsset && ref.Type != ReferenceStyle {
			return ErrOutsideEnvelope{Field: "reference_images.type", Got: ref.Type,
				Admissible: []string{ReferenceAsset, ReferenceStyle}}
		}
	}
	maxN := e.MaxN
	if maxN == 0 {
		maxN = 1
	}
	if r.N < 1 || r.N > maxN {
		return ErrOutsideEnvelope{Field: "n", Got: fmt.Sprint(r.N),
			Admissible: []string{fmt.Sprintf("1 to %d", maxN)}}
	}
	return nil
}

// ResolveAudio settles the tri-state audio flag against what the model can do.
//
// A model that always produces sound resolves an absent flag to on; one that
// never does resolves it to off. An explicit flag is returned as written and
// left for Validate to refuse if the model disagrees -- resolving it silently
// to the possible value would be the same substitution Validate exists to stop.
func (e Envelope) ResolveAudio(r Request) bool {
	if r.Audio != nil {
		return *r.Audio
	}
	return e.Audio == AudioAlways
}

func mergeInts(dst, src []int) []int {
	for _, v := range src {
		if !slices.Contains(dst, v) {
			dst = append(dst, v)
		}
	}
	slices.Sort(dst)
	return dst
}

func mergeStrings(dst, src []string) []string {
	for _, v := range src {
		if !slices.Contains(dst, v) {
			dst = append(dst, v)
		}
	}
	slices.Sort(dst)
	return dst
}

// widerAudio takes the union of two models' audio support: if one can produce
// sound and another cannot, the model as a whole treats it as optional.
func widerAudio(a, b AudioSupport) AudioSupport {
	if a == "" {
		return b
	}
	if b == "" || a == b {
		return a
	}
	return AudioOptional
}

// widerCancel takes the more capable of two cancel modes, because the union
// describes what some route can do. Candidate filtering narrows it again.
func widerCancel(a, b CancelMode) CancelMode {
	rank := map[CancelMode]int{CancelNever: 0, CancelQueuedOnly: 1, CancelAnytime: 2}
	if rank[b] > rank[a] {
		return b
	}
	if a == "" {
		return CancelNever
	}
	return a
}

// durationAdmissible describes what this model would accept, falling back to
// "a positive number of seconds" for a model that has not enumerated its
// durations -- which is still a real answer, and better than an empty one.
func durationAdmissible(e Envelope) []string {
	if len(e.DurationsSeconds) > 0 {
		return intsToStrings(e.DurationsSeconds)
	}
	return []string{"a positive number of seconds"}
}

func intsToStrings(v []int) []string {
	out := make([]string, 0, len(v))
	for _, n := range v {
		out = append(out, fmt.Sprint(n))
	}
	return out
}
