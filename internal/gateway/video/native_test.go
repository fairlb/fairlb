package video

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// A surface with no mapper is a promise that cannot be reached: a request
// arrives in that vendor's shape and there is nothing to send it with
// (ADR-0225). The reverse is a legitimate state -- routable, not switch-to-able
// -- so it is not asserted.
func TestEveryCompatibilitySurfaceHasAMapper(t *testing.T) {
	for _, vendor := range NativeVendors() {
		if _, ok := MapperFor(vendor); !ok {
			t.Errorf("%s publishes a compatibility surface but this build cannot reach that vendor", vendor)
		}
		if _, ok := catalog.LookupVendor(vendor); !ok {
			t.Errorf("%s publishes a compatibility surface but is not in the vendor registry", vendor)
		}
	}
}

// Two routes that answer the same method and path would make which one runs
// depend on mount order, and they mean different things.
func TestNativeRoutesAreUnambiguous(t *testing.T) {
	for _, vendor := range NativeVendors() {
		s, _ := NativeSurfaceFor(vendor)
		seen := map[string]bool{}
		for _, r := range s.Routes() {
			if r.Method == "" || r.Path == "" || r.Kind == "" {
				t.Errorf("%s: a route is missing its method, path or kind: %+v", vendor, r)
			}
			key := r.Method + " " + r.Path
			if seen[key] {
				t.Errorf("%s: %s is declared twice", vendor, key)
			}
			seen[key] = true
		}
	}
}

// nativeCase is one vendor's own request, as its documentation writes it, plus
// what this gateway must read out of it.
type nativeCase struct {
	vendor string
	route  NativeRoute
	body   string
	path   map[string]string
	// priced is what the request says about the axes the job is billed on. Read
	// wrong and the caller is charged for something they did not ask for.
	wantDuration   int
	wantResolution string
	wantPrompt     string
	wantImage      string
	wantModel      string
	// keptOut is this vendor's own spelling of the priced axes. None of them
	// may survive into the passthrough, because the passthrough is forwarded
	// verbatim and would then set a value beside the one that was priced.
	keptOut []string
	// forwarded is a parameter that belongs to this vendor alone. It has to
	// survive all the way to the wire: reaching a model's headline feature is
	// the reason a caller comes to this surface rather than to /v1/videos, and
	// a knob read out of the body and then lost before the upstream call is
	// indistinguishable from one that was never sent.
	forwarded string
	// upstreamModel is what the outbound mapper is given, so the round trip can
	// be run to the wire rather than stopping at the passthrough map.
	upstreamModel string
}

func nativeCases() []nativeCase {
	return []nativeCase{
		{
			vendor: "kuaishou",
			route:  NativeRoute{Method: "POST", Path: "/v1/videos/text2video", Kind: NativeSubmit, Variant: "text2video"},
			body: `{"model_name":"kling-v2-master","prompt":"a cat on a wall","mode":"pro",
				"duration":"10","aspect_ratio":"16:9","cfg_scale":0.5,
				"camera_control":{"type":"simple","config":{"zoom":-5}}}`,
			wantDuration: 10, wantResolution: "1080p", wantPrompt: "a cat on a wall",
			keptOut:   []string{"duration", "mode", "aspect_ratio", "prompt", "model_name"},
			forwarded: "camera_control", upstreamModel: "kling-v2-master",
		},
		{
			vendor: "google",
			route:  NativeRoute{Method: "POST", Path: "/v1beta/models/*", Kind: NativeSubmit},
			path:   map[string]string{"*": "veo-3.1-generate-preview:predictLongRunning"},
			body: `{"instances":[{"prompt":"a cat on a wall"}],
				"parameters":{"aspectRatio":"16:9","resolution":"1080p","durationSeconds":"8",
				"personGeneration":"allow_adult"}}`,
			wantDuration: 8, wantResolution: "1080p", wantPrompt: "a cat on a wall",
			wantModel: "veo-3.1-generate-preview",
			keptOut:   []string{"aspectRatio", "resolution", "durationSeconds", "numberOfVideos"},
			forwarded: "personGeneration", upstreamModel: "veo-3.1-generate-preview",
		},
		{
			vendor: "minimax",
			route:  NativeRoute{Method: "POST", Path: "/v1/video_generation", Kind: NativeSubmit},
			body: `{"model":"MiniMax-Hailuo-2.3","prompt":"a cat on a wall","duration":10,
				"resolution":"1080P","first_frame_image":"https://x/a.png",
				"callback_url":"https://caller.example/hook"}`,
			wantDuration: 10, wantResolution: "1080p", wantPrompt: "a cat on a wall",
			wantImage: "https://x/a.png", wantModel: "MiniMax-Hailuo-2.3",
			keptOut:   []string{"duration", "resolution", "first_frame_image", "prompt", "model"},
			forwarded: "callback_url", upstreamModel: "MiniMax-Hailuo-2.3",
		},
		{
			vendor: "alibaba",
			route: NativeRoute{Method: "POST", Kind: NativeSubmit,
				Path: "/api/v1/services/aigc/video-generation/video-synthesis"},
			body: `{"model":"wan2.7-i2v-2026-04-25",
				"input":{"prompt":"a cat on a wall",
					"media":[{"type":"first_frame","url":"https://x/a.png"}]},
				"parameters":{"resolution":"1080P","duration":8,"prompt_extend":true}}`,
			wantDuration: 8, wantResolution: "1080p", wantPrompt: "a cat on a wall",
			wantImage: "https://x/a.png", wantModel: "wan2.7-i2v-2026-04-25",
			keptOut:   []string{"resolution", "duration", "ratio", "seed"},
			forwarded: "prompt_extend", upstreamModel: "wan2.7-i2v-2026-04-25",
		},
		{
			vendor: "volcengine",
			route:  NativeRoute{Method: "POST", Path: "/api/v3/contents/generations/tasks", Kind: NativeSubmit},
			body: `{"model":"doubao-seedance-1-5-pro","content":[
				{"type":"text","text":"a cat on a wall --resolution 1080p --duration 8 --ratio 16:9"},
				{"type":"image_url","role":"first_frame","image_url":{"url":"https://x/a.png"}}],
				"callback_url":"https://caller.example/hook"}`,
			wantDuration: 8, wantResolution: "1080p", wantPrompt: "a cat on a wall",
			wantImage: "https://x/a.png",
			keptOut:   []string{"content", "model", "duration", "resolution", "ratio"},
			forwarded: "callback_url", upstreamModel: "seedance-1-5-pro",
		},
	}
}

// The round trip that makes switching real: a request written the way that
// vendor documents it has to be read correctly, and a finished job has to come
// back in a shape that vendor's own client can parse.
func TestEachSurfaceReadsItsVendorsOwnRequest(t *testing.T) {
	for _, tc := range nativeCases() {
		t.Run(tc.vendor, func(t *testing.T) {
			s, ok := NativeSurfaceFor(tc.vendor)
			if !ok {
				t.Fatalf("no surface for %s", tc.vendor)
			}
			r, passthrough, err := s.Decode(NativeRequest{
				Route: tc.route, Body: []byte(tc.body), Path: tc.path})
			if err != nil {
				t.Fatal(err)
			}
			if r.DurationSeconds != tc.wantDuration {
				t.Errorf("duration read as %d, want %d", r.DurationSeconds, tc.wantDuration)
			}
			if r.Resolution != tc.wantResolution {
				t.Errorf("resolution read as %q, want %q", r.Resolution, tc.wantResolution)
			}
			if tc.wantPrompt != "" && r.Prompt != tc.wantPrompt {
				t.Errorf("prompt read as %q, want %q", r.Prompt, tc.wantPrompt)
			}
			if tc.wantImage != "" && r.Image != tc.wantImage {
				t.Errorf("image read as %q, want %q", r.Image, tc.wantImage)
			}
			if tc.wantModel != "" && r.Model != tc.wantModel {
				t.Errorf("model read as %q, want %q", r.Model, tc.wantModel)
			}
			if r.N != 1 {
				t.Errorf("n is %d; these APIs produce one clip per request", r.N)
			}
			// Asserted on the wire, not in the passthrough map. Where each
			// surface parks an unrecognised field is its own business; that it
			// reaches the upstream is the promise.
			m, ok := MapperFor(tc.vendor)
			if !ok {
				t.Fatalf("no mapper for %s", tc.vendor)
			}
			out, err := m.Submit(r, tc.upstreamModel, false)
			if err != nil {
				t.Fatalf("the decoded request could not be sent: %v", err)
			}
			if !strings.Contains(string(out.Body), tc.forwarded) {
				t.Errorf("%q never reached the upstream request; a vendor's own knob is the "+
					"reason to use this surface, and one dropped between here and the wire is "+
					"indistinguishable from one never sent: %s", tc.forwarded, out.Body)
			}
			_ = passthrough
		})
	}
}

// The rule that replaced the one vendor_options used to carry, and it is
// stronger here: on this surface the vendor's parameters are first-class, so
// the only thing standing between a caller and a back-door duration is the
// surface reading it into the Request instead of forwarding it.
func TestNoSurfaceForwardsAPricedParameter(t *testing.T) {
	for _, tc := range nativeCases() {
		t.Run(tc.vendor, func(t *testing.T) {
			s, _ := NativeSurfaceFor(tc.vendor)
			_, passthrough, err := s.Decode(NativeRequest{
				Route: tc.route, Body: []byte(tc.body), Path: tc.path})
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range tc.keptOut {
				if _, leaked := passthrough[key]; leaked {
					t.Errorf("%q reached the passthrough; it decides what is generated, and "+
						"forwarding it beside the value that was priced is how a caller is "+
						"charged for one clip and given another", key)
				}
			}
		})
	}
}

// Seedance carries its parameters inside the prompt, so reading them means
// taking them out of the text. Left in, the upstream would receive a second
// copy of every one beside what the outbound mapper writes -- and where the two
// disagreed the upstream would obey the text while the bill followed the
// Request.
func TestSeedanceCommandsAreRemovedFromThePromptTheyWereReadFrom(t *testing.T) {
	s, _ := NativeSurfaceFor("volcengine")
	r, _, err := s.Decode(NativeRequest{
		Route: NativeRoute{Kind: NativeSubmit},
		Body: []byte(`{"model":"seedance-1-5-pro","content":[{"type":"text",
			"text":"a cat --duration 8 --resolution 720p"}]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Prompt, "--") {
		t.Fatalf("the prompt still carries its commands: %q", r.Prompt)
	}
	if r.Prompt != "a cat" {
		t.Errorf("prompt is %q, want %q", r.Prompt, "a cat")
	}
}

// A finished job goes back in the vendor's own shape, at the vendor's own field
// paths, with this gateway's identifier where the vendor's task id goes and
// this deployment's address where the vendor's video URL goes. Both are opaque
// strings to any client, which is what makes the substitution invisible.
func TestEachSurfaceRendersAFinishedJobInItsVendorsShape(t *testing.T) {
	job := NativeJob{
		ID: "vid_01", Model: "kuaishou/kling-v2", Status: StatusCompleted,
		ContentURL: "https://gw.example/v1/videos/vid_01/content", DurationSeconds: 10,
		CreatedAt: time.Unix(1_800_000_000, 0), UpdatedAt: time.Unix(1_800_000_100, 0),
	}
	for _, tc := range []struct {
		vendor  string
		idPath  []string
		urlPath []string
	}{
		{"kuaishou", []string{"data", "task_id"}, []string{"data", "task_result", "videos", "0", "url"}},
		{"volcengine", []string{"id"}, []string{"content", "video_url"}},
		{"google", []string{"name"},
			[]string{"response", "generateVideoResponse", "generatedSamples", "0", "video", "uri"}},
		{"minimax", []string{"task_id"}, nil},
		{"alibaba", []string{"output", "task_id"}, []string{"output", "video_url"}},
	} {
		t.Run(tc.vendor, func(t *testing.T) {
			s, _ := NativeSurfaceFor(tc.vendor)
			status, body, err := s.Render(NativeRoute{Kind: NativePoll}, job)
			if err != nil {
				t.Fatal(err)
			}
			if status != 200 {
				t.Fatalf("status %d", status)
			}
			id := dig(t, body, tc.idPath)
			// Veo's identifier is an operation name rather than a bare id, and
			// its client GETs that name as a path -- so it has to carry ours
			// inside it rather than instead of it.
			if !strings.HasSuffix(id, "vid_01") {
				t.Errorf("%v is %q; a client reads its task id from there", tc.idPath, id)
			}
			if tc.urlPath == nil {
				// This vendor's poll answers a file id, not a URL; the address
				// arrives on its file route, which has its own case below.
				return
			}
			if got := dig(t, body, tc.urlPath); got != job.ContentURL {
				t.Errorf("%v is %q; it must be this deployment's own address, never the "+
					"upstream's (ADR-0222)", tc.urlPath, got)
			}
		})
	}
}

// Status words are the vendor's own, not ours. A client switching over matches
// on those strings, and answering "completed" to an API that says "succeed"
// would leave every one of them polling forever.
func TestEachSurfaceSpeaksItsVendorsStatusVocabulary(t *testing.T) {
	for _, tc := range []struct {
		vendor string
		path   []string
		want   string
	}{
		{"kuaishou", []string{"data", "task_status"}, "succeed"},
		{"volcengine", []string{"status"}, "succeeded"},
		{"minimax", []string{"status"}, "Success"},
		{"alibaba", []string{"output", "task_status"}, "SUCCEEDED"},
	} {
		s, _ := NativeSurfaceFor(tc.vendor)
		_, body, err := s.Render(NativeRoute{Kind: NativePoll}, NativeJob{
			ID: "vid_01", Status: StatusCompleted, ContentURL: "https://x/c",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := dig(t, body, tc.path); got != tc.want {
			t.Errorf("%s says %q for a finished job, want %q", tc.vendor, got, tc.want)
		}
	}
}

// A refusal has to be parseable by the vendor's own client, or its retry logic
// is blind. Every surface answers in that vendor's error shape and keeps the
// message readable.
func TestEachSurfaceRefusesInItsVendorsErrorShape(t *testing.T) {
	for _, vendor := range NativeVendors() {
		s, _ := NativeSurfaceFor(vendor)
		status, body := s.RenderError(NativeRoute{}, 402, "gateway.insufficient_credits",
			"Insufficient credits")
		if status != 402 {
			t.Errorf("%s changed the HTTP status to %d", vendor, status)
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Errorf("%s: the error body is not JSON: %v", vendor, err)
			continue
		}
		if !strings.Contains(string(body), "Insufficient credits") {
			t.Errorf("%s dropped the message: %s", vendor, body)
		}
	}
}

// dig reads a value out of a JSON document by field path, with numeric segments
// indexing arrays. Written here so the assertions above can be spelled as the
// vendor's own documentation spells its response.
func dig(t *testing.T, body []byte, path []string) string {
	t.Helper()
	var cur any
	if err := json.Unmarshal(body, &cur); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	for _, seg := range path {
		switch node := cur.(type) {
		case map[string]any:
			cur = node[seg]
		case []any:
			idx := slices.Index([]string{"0", "1", "2"}, seg)
			if idx < 0 || idx >= len(node) {
				t.Fatalf("no element %q in %s", seg, body)
			}
			cur = node[idx]
		default:
			t.Fatalf("cannot read %q out of %s", seg, body)
		}
	}
	s, _ := cur.(string)
	return s
}

// MiniMax's own schema types the file identifier as int64, so the job's integer
// alias goes there rather than its UUID. A UUID in that field breaks every
// generated client that parses the response into a typed model -- and then
// "change one base URL" would be true of four vendors and not this one.
func TestMinimaxHandsOutAnIntegerIdentifierWhereItsSchemaSaysInteger(t *testing.T) {
	s, _ := NativeSurfaceFor("minimax")
	job := NativeJob{
		ID: "vid_01", Alias: 1_099_511_627_777, Status: StatusCompleted,
		ContentURL: "https://gw.example/v1/videos/vid_01/content",
	}

	_, body, err := s.Render(NativeRoute{Kind: NativePoll}, job)
	if err != nil {
		t.Fatal(err)
	}
	var poll struct {
		TaskID string `json:"task_id"`
		FileID int64  `json:"file_id"`
	}
	if err := json.Unmarshal(body, &poll); err != nil {
		t.Fatalf("a typed client cannot parse the poll answer: %v (%s)", err, body)
	}
	if poll.FileID != job.Alias {
		t.Errorf("file_id is %d, want the job's alias %d", poll.FileID, job.Alias)
	}
	if poll.TaskID != job.ID {
		t.Errorf("task_id is %q; this vendor's task id is a string, so ours goes there as it is",
			poll.TaskID)
	}

	_, body, err = s.Render(NativeRoute{Kind: NativeArtifact}, job)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		File struct {
			FileID      int64  `json:"file_id"`
			DownloadURL string `json:"download_url"`
		} `json:"file"`
	}
	if err := json.Unmarshal(body, &file); err != nil {
		t.Fatalf("a typed client cannot parse the file record: %v (%s)", err, body)
	}
	if file.File.FileID != job.Alias {
		t.Errorf("the file record's id is %d, want %d", file.File.FileID, job.Alias)
	}
	if file.File.DownloadURL != job.ContentURL {
		t.Errorf("download_url is %q; it must be this deployment's own address (ADR-0222)",
			file.File.DownloadURL)
	}
}

// A default this gateway applies for its own contract must not overrule a
// choice the caller made on the vendor's own one. Two of these mappers set a
// default that changes what is generated -- the prompt optimiser and the
// watermark -- and on a compatibility surface the caller is entitled to ask for
// either.
func TestAVendorsOwnDefaultsAreDefaultedNotForced(t *testing.T) {
	for _, tc := range []struct {
		vendor, model, key string
		at                 []string
	}{
		{"minimax", "MiniMax-Hailuo-2.3", "prompt_optimizer", nil},
		{"alibaba", "wan2.7-t2v", "watermark", []string{"parameters"}},
	} {
		t.Run(tc.vendor, func(t *testing.T) {
			m, _ := MapperFor(tc.vendor)
			base := Request{Prompt: "a cat", DurationSeconds: 6, Resolution: "720p", N: 1}

			out, err := m.Submit(base, tc.model, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := boolAt(t, out.Body, tc.at, tc.key); got != false {
				t.Errorf("a request that said nothing got %s=%v; the default is off", tc.key, got)
			}

			// Written where that surface parks it, which for one of these is
			// inside the sub-object the mapper builds.
			chosen := base
			chosen.Passthrough = map[string]json.RawMessage{tc.key: json.RawMessage("true")}
			if len(tc.at) > 0 {
				chosen.Passthrough = map[string]json.RawMessage{
					tc.at[0]: json.RawMessage(`{"` + tc.key + `":true}`),
				}
			}
			out, err = m.Submit(chosen, tc.model, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := boolAt(t, out.Body, tc.at, tc.key); got != true {
				t.Errorf("a caller asked for %s on this vendor's own surface and got %v; "+
					"their choice about their own API outranks our default for ours", tc.key, got)
			}
		})
	}
}

func boolAt(t *testing.T, body []byte, path []string, key string) bool {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	cur := doc
	for _, seg := range path {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			t.Fatalf("no object at %q in %s", seg, body)
		}
		cur = next
	}
	v, present := cur[key]
	if !present {
		t.Fatalf("%q is absent from %s", key, body)
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return b == "true"
	default:
		// A passthrough value arrives as raw JSON and marshals back as itself.
		return strings.Contains(string(body), `"`+key+`":true`)
	}
}

// Nothing a caller writes may vanish without a word.
//
// This is the assertion that was missing while three surfaces were silently
// dropping fields: TestNoSurfaceForwardsAPricedParameter checks that priced
// keys are *absent* from the passthrough, and nothing checked that unpriced
// ones *survive*. A field this gateway does not recognise has two honest
// outcomes -- forward it, or refuse the request naming it -- and the failure it
// must never have is the third one.
func TestNoSurfaceSilentlyDropsAFieldTheCallerWrote(t *testing.T) {
	for _, tc := range nativeCases() {
		t.Run(tc.vendor, func(t *testing.T) {
			s, _ := NativeSurfaceFor(tc.vendor)
			m, _ := MapperFor(tc.vendor)
			for _, probe := range nativeProbes(tc) {
				r, _, err := s.Decode(NativeRequest{
					Route: tc.route, Body: []byte(probe.body), Path: tc.path})
				if err != nil {
					if !strings.Contains(err.Error(), probe.marker) {
						t.Errorf("%s: refused without naming the field: %v", probe.where, err)
					}
					continue // refused, and it said what it could not read
				}
				out, err := m.Submit(r, tc.upstreamModel, false)
				if err != nil {
					continue // refused later, still not silently
				}
				if !strings.Contains(string(out.Body), probe.marker) {
					t.Errorf("%s: %q vanished between the request and the wire: %s",
						probe.where, probe.marker, out.Body)
				}
			}
		})
	}
}

// nativeProbe is one made-up field, placed somewhere a caller could plausibly
// write it in that vendor's own body.
type nativeProbe struct {
	where  string
	marker string
	body   string
}

// nativeProbes plants a marker in each place that vendor's body has room for
// one this build has never heard of.
func nativeProbes(tc nativeCase) []nativeProbe {
	const marker = "fairlbProbeField"
	switch tc.vendor {
	case "kuaishou", "minimax":
		return []nativeProbe{{"top level", marker,
			injectTopLevel(tc.body, marker)}}
	case "volcengine":
		return []nativeProbe{
			{"top level", marker, injectTopLevel(tc.body, marker)},
			{"inline command", "--" + strings.ToLower(marker),
				`{"model":"seedance-1-5-pro","content":[{"type":"text",
				  "text":"a cat --duration 8 --` + strings.ToLower(marker) + ` true"}]}`},
		}
	case "alibaba":
		return []nativeProbe{
			{"top level", marker, injectTopLevel(tc.body, marker)},
			{"input", marker, `{"model":"wan2.7-i2v-2026-04-25",
			  "input":{"prompt":"a cat","` + marker + `":"x"},"parameters":{"duration":8}}`},
		}
	case "google":
		return []nativeProbe{
			{"top level", marker, injectTopLevel(tc.body, marker)},
			{"instance", marker, `{"instances":[{"prompt":"a cat","` + marker + `":"x"}],
			  "parameters":{"durationSeconds":"8"}}`},
		}
	default:
		panic("nativeProbes has no probe for " + tc.vendor)
	}
}

// injectTopLevel adds one unknown key to a JSON object body.
func injectTopLevel(body, marker string) string {
	return `{"` + marker + `":"x",` + strings.TrimSpace(body)[1:]
}
