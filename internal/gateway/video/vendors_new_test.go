package video

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// The two registries have to agree. The vendor registry decides which channels
// an operator can declare `video` on; this one decides which of those the data
// plane can actually shape a request for. A vendor in the first and not the
// second saves fine, serves never, and shows as configured -- the failure
// ADR-0178 exists to catch at configuration time rather than at 404 time.
func TestTheVendorRegistryAndTheMapperRegistryAgree(t *testing.T) {
	declared := map[string]bool{}
	for _, v := range catalog.Vendors() {
		if slices.Contains(v.Protocols, catalog.ProtocolVideo) {
			declared[v.Slug] = true
		}
	}
	for slug := range declared {
		if _, ok := MapperFor(slug); !ok {
			t.Errorf("the vendor registry offers %q on the video plane, but this build has no mapper "+
				"for it: a channel declared that way saves, never serves, and shows as configured", slug)
		}
	}
	for _, slug := range Vendors() {
		if !declared[slug] {
			t.Errorf("%q has a video mapper but the vendor registry does not publish the video "+
				"protocol for it, so no operator can wire one", slug)
		}
	}
}

// MiniMax's poll answers a file id rather than a link, so its artifact is two
// calls. The first is to this vendor's own API and carries our credential; the
// second is to a signed link that carries its own, and attaching ours there
// would hand the upstream key to a CDN.
func TestMinimaxArtifactIsTwoHopsAndOnlyTheFirstIsOurs(t *testing.T) {
	m := minimaxMapper{}
	p, err := m.PollResult(200, []byte(
		`{"task_id":"115334141465231360","status":"Success","file_id":205258526306433,
		  "base_resp":{"status_code":0,"status_msg":"success"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusCompleted {
		t.Fatalf("status %q, want completed", p.Status)
	}
	// The id arrives unquoted in this vendor's own example and as a string in
	// others. Either has to survive.
	if p.ArtifactRef != "205258526306433" {
		t.Fatalf("file id read as %q", p.ArtifactRef)
	}

	first, err := m.Artifact(p)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Indirect {
		t.Fatal("the first hop returns a file record, not bytes; it must say so")
	}
	if !first.NeedsUpstreamCredential {
		t.Error("the file API is this vendor's own and needs our key")
	}
	if first.Request.Path != "/v1/files/retrieve" || first.Request.Query["file_id"] != "205258526306433" {
		t.Fatalf("first hop is %s?%v", first.Request.Path, first.Request.Query)
	}

	second, err := m.ResolveArtifact(200, []byte(
		`{"file":{"file_id":205258526306433,"bytes":5896337,"filename":"out.mp4",
		  "download_url":"https://cdn.example.com/out.mp4?sig=abc"},
		  "base_resp":{"status_code":0,"status_msg":"success"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if second.Indirect {
		t.Fatal("the second hop must be the last; chaining would make the fetch unbounded")
	}
	if second.NeedsUpstreamCredential {
		t.Error("the download link is signed and carries its own authorisation; " +
			"attaching ours would send the upstream key to a CDN")
	}
	if second.Request.URL != "https://cdn.example.com/out.mp4?sig=abc" {
		t.Fatalf("second hop goes to %q", second.Request.URL)
	}
}

// A 200 carrying a non-zero code is how this vendor refuses. Reading only the
// HTTP status would take a refusal for a submitted job and then poll a task id
// that does not exist.
func TestMinimaxRefusalIsNotAHTTPError(t *testing.T) {
	if _, err := (minimaxMapper{}).SubmitResult(Request{}, 200,
		[]byte(`{"base_resp":{"status_code":1008,"status_msg":"insufficient balance"}}`)); err == nil {
		t.Fatal("a 200 carrying a refusal code was read as a successful submit")
	}
}

// A content refusal is a terminal verdict on the job; a rate limit is not. The
// difference decides whether the hold is released or the job is polled again,
// so it cannot be collapsed into "the poll failed".
func TestMinimaxSeparatesAContentRefusalFromATransportFailure(t *testing.T) {
	m := minimaxMapper{}

	p, err := m.PollResult(200, []byte(
		`{"base_resp":{"status_code":1026,"status_msg":"sensitive content"}}`))
	if err != nil {
		t.Fatalf("a content refusal is an answer, not an error: %v", err)
	}
	if p.Status != StatusFailed || p.ErrorMessage != "sensitive content" {
		t.Errorf("a content refusal must be terminal and keep its reason, got %+v", p)
	}

	if _, err := m.PollResult(200, []byte(
		`{"base_resp":{"status_code":1002,"status_msg":"rate limited"}}`)); err == nil {
		t.Error("a rate limit was reported as a verdict on the job; it would void a hold " +
			"for a clip the upstream is still making")
	}
}

// The prompt this plane sends is the prompt the caller wrote. This vendor
// rewrites it by default, so the switch is sent explicitly off.
func TestMinimaxDoesNotLetTheUpstreamRewriteThePrompt(t *testing.T) {
	out, err := (minimaxMapper{}).Submit(Request{
		Prompt: "a cat", DurationSeconds: 6, Resolution: "1080p", N: 1,
	}, "MiniMax-Hailuo-2.3", false)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["prompt_optimizer"] != false {
		t.Errorf("prompt_optimizer is %#v; this API defaults it on, which rewrites the caller's prompt",
			doc["prompt_optimizer"])
	}
	// This API spells the tier with a capital P.
	if doc["resolution"] != "1080P" {
		t.Errorf("resolution rendered as %#v, want 1080P", doc["resolution"])
	}
}

// Without the async header this vendor's create path blocks until the clip is
// finished, which on this plane is a request that outlives every timeout
// between here and the caller.
func TestWanAsksForTheAsynchronousForm(t *testing.T) {
	out, err := (alibabaMapper{}).Submit(Request{
		Prompt: "a cat", DurationSeconds: 5, Resolution: "1080p", AspectRatio: "16:9", N: 1,
	}, "wan2.7-t2v", false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Headers["X-DashScope-Async"] != "enable" {
		t.Fatalf("the async header is missing: %v", out.Headers)
	}
	var doc struct {
		Parameters map[string]any `json:"parameters"`
	}
	if err := json.Unmarshal(out.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Parameters["resolution"] != "1080P" {
		t.Errorf("resolution rendered as %#v, want 1080P", doc.Parameters["resolution"])
	}
	if doc.Parameters["ratio"] != "16:9" {
		t.Errorf("the text model takes a ratio, got %#v", doc.Parameters["ratio"])
	}
	if doc.Parameters["watermark"] != false {
		t.Error("the watermark must be sent explicitly off; this vendor's default has moved before")
	}
}

// The image-to-video models take no ratio -- the shape comes from the first
// frame. Declaring one for them would accept a value the upstream ignores,
// which is a clip the caller did not ask for.
func TestWanImageToVideoDeclaresNoAspectRatio(t *testing.T) {
	m := alibabaMapper{}
	if len(m.Envelope("wan2.7-i2v-2026-04-25").AspectRatios) != 0 {
		t.Error("the image-to-video envelope declares aspect ratios the upstream does not take")
	}
	if len(m.Envelope("wan2.7-t2v").AspectRatios) == 0 {
		t.Error("the text-to-video envelope must still offer the ratios it does take")
	}

	out, err := m.Submit(Request{
		Prompt: "a cat", DurationSeconds: 5, Resolution: "720p",
		Image: "https://x/a.png", LastFrame: "https://x/b.png", N: 1,
	}, "wan2.7-i2v-2026-04-25", false)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Input struct {
			Media []struct {
				Type string `json:"type"`
				URL  string `json:"url"`
			} `json:"media"`
		} `json:"input"`
	}
	if err := json.Unmarshal(out.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Input.Media) != 2 ||
		doc.Input.Media[0].Type != "first_frame" || doc.Input.Media[1].Type != "last_frame" {
		t.Fatalf("the two image roles did not arrive typed: %+v", doc.Input.Media)
	}
}

// Duration is a range on this vendor and an enumeration in the envelope. Both
// ends have to be admissible and one past either end has to be refused, with
// the admissible set named -- a caller cannot correct what they cannot see.
func TestWanAdmitsItsWholeDurationRangeAndNothingOutsideIt(t *testing.T) {
	e := (alibabaMapper{}).Envelope("wan2.7-t2v")
	for _, seconds := range []int{2, 8, 15} {
		r := Request{Model: "m", Prompt: "a cat", DurationSeconds: seconds, Resolution: "720p", N: 1}
		if err := e.Validate(r, false); err != nil {
			t.Errorf("duration_seconds=%d must be admissible: %v", seconds, err)
		}
	}
	for _, seconds := range []int{0, 1, 16} {
		r := Request{Model: "m", Prompt: "a cat", DurationSeconds: seconds, Resolution: "720p", N: 1}
		var outside ErrOutsideEnvelope
		if err := e.Validate(r, false); !errors.As(err, &outside) {
			t.Errorf("duration_seconds=%d must be refused, got %v", seconds, err)
		} else if !strings.Contains(outside.Error(), "15") {
			t.Errorf("the refusal must name what is admissible, got %q", outside.Error())
		}
	}
}

// UNKNOWN means the task id has aged out, not that generation failed. It is the
// same fact a 404 carries elsewhere and has to be reported the same way, or a
// job the vendor has forgotten looks like a job the vendor refused.
func TestWanReadsAnExpiredTaskAsForgottenRatherThanRefused(t *testing.T) {
	p, err := (alibabaMapper{}).PollResult(200,
		[]byte(`{"output":{"task_id":"a-1","task_status":"UNKNOWN"},"request_id":"r-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !p.NotFound {
		t.Error("an aged-out task id must be reported as not-found, so the worker needs two readings")
	}
}
