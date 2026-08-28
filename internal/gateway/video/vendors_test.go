package video

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The vendors were chosen to span the shape space rather than by prominence:
// if the interface can hold them, the next one is one file. This asserts that
// they really do differ on the axes the interface has -- if they ever stop
// differing, the interface has been shaped by a coincidence.
func TestTheRegisteredVendorsSpanTheShapeSpace(t *testing.T) {
	cancels := map[string]CancelMode{}
	credentials := map[string]bool{}
	for _, vendor := range Vendors() {
		m, _ := MapperFor(vendor)
		cancels[vendor] = m.CancelMode()
		a, err := m.Artifact(Poll{ArtifactRef: "https://x/v.mp4"})
		if err != nil {
			t.Fatalf("%s: %v", vendor, err)
		}
		credentials[vendor] = a.NeedsUpstreamCredential
	}
	// Cancel is deliberately *not* asserted to differ. It used to, on the
	// strength of a DELETE that turned out to belong to a third-party relay
	// rather than to Kling's own platform. Today no upstream in this registry
	// publishes a cancel at all, and that is a fact about the market rather
	// than about this interface -- CancelMode stays three-valued because the
	// envelope is the operator's to declare (ADR-0221). If a vendor with a real
	// cancel is ever added, put the assertion back.
	for vendor, mode := range cancels {
		if mode != CancelNever {
			t.Errorf("%s claims it can cancel; no upstream here publishes one, "+
				"so a bundled prefill promising it is a button that always fails", vendor)
		}
	}
	if !credentials["google"] {
		t.Error("Google's file URI is credential-authenticated")
	}
	if credentials["kuaishou"] || credentials["volcengine"] {
		t.Error("a presigned CDN link must not be fetched with the upstream credential attached")
	}
	// Durations barely overlap, which is the fact that justifies declaring the
	// admissible set per route rather than picking a common one.
	g := (googleMapper{}).Envelope("veo").DurationsSeconds
	k := (kuaishouMapper{}).Envelope("kling").DurationsSeconds
	if len(g) == 0 || len(k) == 0 {
		t.Fatal("a vendor declares no durations")
	}
	for _, d := range g {
		for _, other := range k {
			if d == other {
				t.Errorf("google and kuaishou both accept %ds; the fixture no longer reflects the real APIs", d)
			}
		}
	}
}

// Kling has no resolution field. The mapping to `mode` is real, and it must be
// visible in the request rather than implied.
func TestKlingResolutionBecomesMode(t *testing.T) {
	for _, tc := range []struct{ resolution, mode string }{
		{"720p", "std"}, {"1080p", "pro"}, {"", "std"},
	} {
		out, err := (kuaishouMapper{}).Submit(Request{
			Prompt: "a cat", DurationSeconds: 5, Resolution: tc.resolution, N: 1,
		}, "kling-v2", false)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(out.Body, &doc); err != nil {
			t.Fatal(err)
		}
		if doc["mode"] != tc.mode {
			t.Errorf("resolution %q became mode %v, want %q", tc.resolution, doc["mode"], tc.mode)
		}
		if _, sent := doc["resolution"]; sent {
			t.Error("a resolution field was sent to an API that has none")
		}
		// Duration is a string on this API.
		if doc["duration"] != "5" {
			t.Errorf("duration rendered as %#v, want the string \"5\"", doc["duration"])
		}
	}
}

// The end frame is a pro-mode feature. Refusing where the caller can still act
// on it beats a job that fails minutes later, and beats dropping the frame and
// producing something they did not ask for.
func TestKlingRefusesAnEndFrameOutsideProMode(t *testing.T) {
	r := Request{Prompt: "a cat", DurationSeconds: 5, Resolution: "720p",
		Image: "https://x/a.png", LastFrame: "https://x/b.png", N: 1}
	if _, err := (kuaishouMapper{}).Submit(r, "kling-v2", false); err == nil {
		t.Fatal("an end frame was accepted at 720p, where the upstream does not support it")
	}
	r.Resolution = "1080p"
	out, err := (kuaishouMapper{}).Submit(r, "kling-v2", false)
	if err != nil {
		t.Fatalf("an end frame must be accepted at 1080p: %v", err)
	}
	if !strings.Contains(string(out.Body), "image_tail") {
		t.Fatalf("the end frame did not reach the request: %s", out.Body)
	}
}

// This API answers 200 with a non-zero code for a refusal, so the HTTP status
// alone is not the verdict. Reading only the status would take a refusal for a
// submitted job and then poll a task id that does not exist.
func TestKlingRefusalIsNotAHTTPError(t *testing.T) {
	if _, err := (kuaishouMapper{}).SubmitResult(Request{}, 200,
		[]byte(`{"code":1101,"message":"account arrears"}`)); err == nil {
		t.Fatal("a 200 carrying a refusal code was read as a successful submit")
	}
}

// Seedance carries generation parameters inside the prompt text. Building them
// from the normalised request is the point: a caller writing their own
// --duration would get a clip priced at something else.
func TestSeedanceEmbedsParametersAsCommands(t *testing.T) {
	out, err := (volcengineMapper{}).Submit(Request{
		Prompt: "a cat", DurationSeconds: 8, Resolution: "1080p", AspectRatio: "16:9", N: 1,
	}, "seedance-1-0-pro", false)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out.Body, &doc); err != nil {
		t.Fatal(err)
	}
	text := doc.Content[0].Text
	for _, want := range []string{"--resolution 1080p", "--duration 8", "--ratio 16:9"} {
		if !strings.Contains(text, want) {
			t.Errorf("the prompt is missing %q: %q", want, text)
		}
	}
}

// Success with nothing to fetch would settle a charge for a video that does not
// exist. Every vendor has to treat it as a failure.
func TestEveryVendorTreatsEmptySuccessAsFailure(t *testing.T) {
	for _, tc := range []struct {
		vendor, body string
	}{
		{"volcengine", `{"id":"t","status":"succeeded","content":{"video_url":""}}`},
		{"kuaishou", `{"code":0,"data":{"task_id":"t","task_status":"succeed","task_result":{"videos":[]}}}`},
	} {
		m, _ := MapperFor(tc.vendor)
		p, err := m.PollResult(200, []byte(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.vendor, err)
		}
		if p.Status != StatusFailed {
			t.Errorf("%s reported %q for a success with no video; it would settle a charge for nothing",
				tc.vendor, p.Status)
		}
	}
}

// Every registered vendor must be able to answer the whole interface. A mapper
// that panics or returns an unusable shape on one method is a vendor that
// half-works, and the half that does not is discovered in production.
func TestEveryRegisteredVendorAnswersTheWholeInterface(t *testing.T) {
	if len(Vendors()) < 3 {
		t.Fatalf("only %d vendors registered", len(Vendors()))
	}
	req := Request{Prompt: "a cat", DurationSeconds: 5, N: 1}
	for _, vendor := range Vendors() {
		t.Run(vendor, func(t *testing.T) {
			m, ok := MapperFor(vendor)
			if !ok {
				t.Fatal("registered but not resolvable")
			}
			e := m.Envelope("model-x")
			if !e.Configured() {
				t.Fatal("the prefill envelope declares nothing")
			}
			if e.MaxJobSeconds <= 0 {
				t.Error("no job ceiling, so the hold TTL has nothing to size itself from")
			}
			r := req
			r.DurationSeconds = e.DurationsSeconds[0]
			if len(e.Resolutions) > 0 {
				r.Resolution = e.Resolutions[0]
			}
			out, err := m.Submit(r, "model-x", e.ResolveAudio(r))
			if err != nil {
				t.Fatalf("cannot shape a submit for its own envelope: %v", err)
			}
			// Round-tripping through SubmitResult is what produces a usable
			// identifier: on one vendor the id alone does not address the job.
			id, err := m.SubmitResult(r, 200, submitEcho(vendor))
			if err != nil {
				t.Fatalf("cannot read its own submit response: %v", err)
			}
			if _, err := m.Poll(id); err != nil {
				t.Fatalf("cannot shape a poll for the id it just returned: %v", err)
			}
			if _, err := m.Poll(""); err == nil {
				t.Error("polling with no upstream id must be refused, not sent")
			}
			_ = out
			if m.CancelMode() != CancelNever {
				if _, err := m.Cancel(id); err != nil {
					t.Errorf("declares it can cancel but cannot shape one: %v", err)
				}
			}
			if _, err := m.Artifact(Poll{}); err == nil {
				t.Error("returned an artifact request for a poll with nothing to fetch")
			}
		})
	}
}

// submitEcho is the shortest successful create response each vendor publishes.
//
// Table-driven from the vendors' own documented shapes rather than generated,
// because the point of the walk above is that a mapper can read what its
// upstream actually answers -- a synthetic body every mapper accepts would
// prove nothing.
func submitEcho(vendor string) []byte {
	switch vendor {
	case "google":
		return []byte(`{"name":"models/model-x/operations/op-1"}`)
	case "kuaishou":
		return []byte(`{"code":0,"data":{"task_id":"t-1","task_status":"submitted"}}`)
	case "volcengine":
		return []byte(`{"id":"cgt-1","status":"queued"}`)
	case "minimax":
		return []byte(`{"task_id":"115334141465231360","base_resp":{"status_code":0,"status_msg":"success"}}`)
	case "alibaba":
		return []byte(`{"output":{"task_id":"a-1","task_status":"PENDING"},"request_id":"r-1"}`)
	default:
		panic("submitEcho has no create response for vendor " + vendor +
			"; a registered vendor with no documented shape here cannot be walked")
	}
}

// The resolved audio flag, not the tri-state field on the request, is what a
// mapper must send. They differ exactly when an operator has declared a model
// `audio: always` and the caller said nothing -- and in that case the job has
// already been priced with sound, so a mapper reading the request would produce
// a silent clip billed at the scored rate.
func TestTheMapperSendsTheAudioTheJobWasPricedOn(t *testing.T) {
	always := Envelope{
		DurationsSeconds: []int{8}, Resolutions: []string{"1080p"}, Audio: AudioAlways,
	}
	r := Request{Prompt: "a cat", DurationSeconds: 8, Resolution: "1080p", N: 1}
	audioOn := always.ResolveAudio(r)
	if !audioOn {
		t.Fatal("a model that always produces sound must resolve an absent flag to on")
	}
	if r.Audio != nil {
		t.Fatal("the fixture must leave the wire field absent; that is the case that differs")
	}

	out, err := (volcengineMapper{}).Submit(r, "seedance-1-5-pro", audioOn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out.Body), "--audio true") {
		t.Errorf("the request was priced with sound and sent without it: %s", out.Body)
	}

	// And the other direction: priced silent, sent silent.
	out, err = (volcengineMapper{}).Submit(r, "seedance-1-5-pro", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out.Body), "--audio") {
		t.Errorf("the request was priced silent and sent with sound: %s", out.Body)
	}
}

// Kling reads a job back at the path of the task type that created it. Polling
// an image-to-video job at the text2video path answers 404 forever, which the
// job machinery reads as a forgotten job and eventually voids the hold -- for a
// clip the upstream generated and charged its own account for.
func TestKlingReadsAJobBackOnThePathThatCreatedIt(t *testing.T) {
	m := kuaishouMapper{}
	for _, tc := range []struct{ name, image, want string }{
		{"text to video", "", "text2video"},
		{"image to video", "https://x/a.png", "image2video"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Request{Prompt: "a cat", DurationSeconds: 5, Resolution: "720p",
				Image: tc.image, N: 1}
			out, err := m.Submit(r, "kling-v2", false)
			if err != nil {
				t.Fatal(err)
			}
			if out.Path != "/v1/videos/"+tc.want {
				t.Fatalf("created at %s, want /v1/videos/%s", out.Path, tc.want)
			}
			id, err := m.SubmitResult(r, 200,
				[]byte(`{"code":0,"data":{"task_id":"t-1","task_status":"submitted"}}`))
			if err != nil {
				t.Fatal(err)
			}
			poll, err := m.Poll(id)
			if err != nil {
				t.Fatal(err)
			}
			if poll.Path != "/v1/videos/"+tc.want+"/t-1" {
				t.Errorf("polled at %s, want /v1/videos/%s/t-1", poll.Path, tc.want)
			}
		})
	}
}

// A bare id names no task type, so it cannot address a job on this vendor.
// Refusing beats sending it to /v1/videos/<id>, which is not an endpoint Kling
// publishes: that 404 is indistinguishable from a job the vendor has forgotten.
func TestKlingRefusesAnIdentifierWithNoTaskType(t *testing.T) {
	for _, id := range []string{"", "t-1", "lipsync/t-1", "text2video/"} {
		if _, err := (kuaishouMapper{}).Poll(id); err == nil {
			t.Errorf("polling accepted %q, which addresses no job", id)
		}
	}
}

// Only a vendor whose artifact really is two hops may answer ResolveArtifact.
// A mapper that returned something usable here without setting Indirect would
// be a second, unreachable code path.
func TestOnlyIndirectVendorsResolveAnArtifact(t *testing.T) {
	for _, vendor := range Vendors() {
		m, _ := MapperFor(vendor)
		art, err := m.Artifact(Poll{ArtifactRef: "https://x/v.mp4", ContentType: "video/mp4"})
		if err != nil {
			t.Fatalf("%s: %v", vendor, err)
		}
		_, err = m.ResolveArtifact(200, []byte(`{}`))
		if art.Indirect && errors.Is(err, ErrArtifactIsDirect) {
			t.Errorf("%s declares a two-hop artifact but refuses to resolve one", vendor)
		}
		if !art.Indirect && err == nil {
			t.Errorf("%s fetches its artifact in one hop but answered a resolve", vendor)
		}
	}
}
