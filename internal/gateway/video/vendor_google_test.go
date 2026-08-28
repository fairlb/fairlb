package video

import (
	"encoding/json"
	"strings"
	"testing"
)

func googleReq() Request {
	return Request{
		Model: "google/veo-3.1", Prompt: "a cat on a wall",
		DurationSeconds: 8, Resolution: "720p", AspectRatio: "16:9", N: 1,
	}
}

func TestGoogleSubmitRendersTheLongRunningCall(t *testing.T) {
	out, err := googleMapper{}.Submit(googleReq(), "veo-3.1-generate-preview", true)
	if err != nil {
		t.Fatal(err)
	}
	if out.Method != "POST" {
		t.Fatalf("method %q", out.Method)
	}
	if want := "/v1beta/models/veo-3.1-generate-preview:predictLongRunning"; out.Path != want {
		t.Fatalf("path %q, want %q", out.Path, want)
	}
	var doc struct {
		Instances []struct {
			Prompt string `json:"prompt"`
		} `json:"instances"`
		Parameters map[string]any `json:"parameters"`
	}
	if err := json.Unmarshal(out.Body, &doc); err != nil {
		t.Fatal(err)
	}
	// Duration is a string enum on this API, not a number. Sending 8 where "8"
	// is expected is rejected upstream, and the failure would arrive minutes
	// later as a failed job rather than at submit.
	if got := doc.Parameters["durationSeconds"]; got != "8" {
		t.Fatalf("durationSeconds rendered as %#v, want the string \"8\"", got)
	}
	if doc.Parameters["aspectRatio"] != "16:9" || doc.Parameters["resolution"] != "720p" {
		t.Fatalf("parameters lost a value: %#v", doc.Parameters)
	}
	if doc.Instances[0].Prompt != "a cat on a wall" {
		t.Fatalf("prompt lost: %#v", doc.Instances)
	}
	// Audio is native and has no parameter; sending one is an error upstream.
	if _, sent := doc.Parameters["generateAudio"]; sent {
		t.Error("generateAudio was sent; Veo generates audio natively and rejects the parameter")
	}
}

// An https image reference cannot be fetched by this API. Dropping it would
// generate a text-to-video clip and bill for it, so it is refused.
func TestGoogleRefusesAnImageItCannotFetch(t *testing.T) {
	r := googleReq()
	r.Image = "https://example.test/a.png"
	if _, err := (googleMapper{}).Submit(r, "veo-3.1", true); err == nil {
		t.Fatal("an unfetchable image reference was accepted and would have been silently dropped")
	}
	r.Image = "data:image/png;base64,AAAA"
	out, err := googleMapper{}.Submit(r, "veo-3.1", true)
	if err != nil {
		t.Fatalf("a data URL must be accepted: %v", err)
	}
	if !strings.Contains(string(out.Body), "bytesBase64Encoded") {
		t.Fatalf("the image did not reach the request: %s", out.Body)
	}
}

func TestGooglePollNormalisesEveryTerminalShape(t *testing.T) {
	m := googleMapper{}
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		want     Status
		notFound bool
		wantURL  string
	}{
		{"still running", 200, `{"name":"models/x/operations/1","done":false}`, StatusInProgress, false, ""},
		{"done with a video", 200,
			`{"name":"n","done":true,"response":{"generateVideoResponse":{"generatedSamples":[{"video":{"uri":"https://g/v.mp4","mimeType":"video/mp4"}}]}}}`,
			StatusCompleted, false, "https://g/v.mp4"},
		{"error", 200, `{"name":"n","done":true,"error":{"code":3,"message":"bad prompt","status":"INVALID_ARGUMENT"}}`,
			StatusFailed, false, ""},
		{"forgotten", 404, `{}`, StatusFailed, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := m.PollResult(tc.status, []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if p.Status != tc.want {
				t.Fatalf("status %q, want %q", p.Status, tc.want)
			}
			if p.NotFound != tc.notFound {
				t.Fatalf("notFound %v, want %v", p.NotFound, tc.notFound)
			}
			if p.ArtifactRef != tc.wantURL {
				t.Fatalf("download url %q, want %q", p.ArtifactRef, tc.wantURL)
			}
		})
	}
}

// Done, no error and no sample is what a safety filter looks like on this API.
// Reported as success it would settle a charge for a video that does not exist.
func TestGoogleTreatsAnEmptyCompletionAsAFailure(t *testing.T) {
	p, err := googleMapper{}.PollResult(200, []byte(
		`{"name":"n","done":true,"response":{"generateVideoResponse":{"generatedSamples":[],"raiMediaFilteredReasons":["blocked: person"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusFailed {
		t.Fatalf("an empty completion reported %q; it would settle a charge for a video that does not exist", p.Status)
	}
	if !strings.Contains(p.ErrorMessage, "blocked: person") {
		t.Fatalf("the filter's own reason must reach the caller, got %q", p.ErrorMessage)
	}
}

// The download is served by Google and authenticated as the upstream account.
// Getting this wrong either leaks the key to a CDN or fails every fetch.
func TestGoogleArtifactNeedsTheUpstreamCredential(t *testing.T) {
	a, err := googleMapper{}.Artifact(Poll{ArtifactRef: "https://g/v.mp4", ContentType: "video/mp4"})
	if err != nil {
		t.Fatal(err)
	}
	if !a.NeedsUpstreamCredential {
		t.Fatal("Google's file URI is credential-authenticated; fetching it anonymously fails")
	}
}

func TestGoogleCancelIsRefusedRatherThanFaked(t *testing.T) {
	m := googleMapper{}
	if m.CancelMode() != CancelNever {
		t.Fatalf("cancel mode %q; the Gemini API documents no cancel for this operation", m.CancelMode())
	}
	if _, err := m.Cancel("models/x/operations/1"); err == nil {
		t.Fatal("cancel returned a request for an operation that cannot be cancelled")
	}
}
