package proxy

import (
	"bytes"
	"testing"

	"github.com/fairlb/fairlb/internal/gateway/catalog"
)

// ParseUsage's doc comment says "every surface must appear in this switch by
// name", and until now nothing held it to that. The comment also records what
// happens when the rule is broken: a surface that falls through to the default
// arm is parsed by the *wrong* arm, comes back Present with every count at
// zero, and the request bills at nothing. That is not hypothetical -- it is
// what the images surface did until it was given its own arm.
//
// So this is the gate for that sentence. Every surface must be classified here
// deliberately, and a surface added without a decision fails the test rather
// than quietly billing at zero.
func TestEverySurfaceIsClassifiedForUsageParsing(t *testing.T) {
	// What each surface's usage looks like when the upstream reports none.
	// `true` means ParseUsage has a dialect arm that can report usage for this
	// surface; `false` means the surface never carries an upstream usage object
	// and its charge comes from somewhere else.
	reportsUpstreamUsage := map[catalog.Surface]bool{
		catalog.SurfaceChat:                     true,
		catalog.SurfaceMessages:                 true,
		catalog.SurfaceMessagesCountTokens:      true,
		catalog.SurfaceResponses:                true,
		catalog.SurfaceResponsesCompact:         true,
		catalog.SurfaceResponsesResources:       true,
		catalog.SurfaceResponsesInputTokens:     true,
		catalog.SurfaceEmbeddings:               true,
		catalog.SurfaceImages:                   true,
		catalog.SurfaceImagesEdit:               true,
		catalog.SurfaceGenerateContent:          true,
		catalog.SurfaceGeminiCountTokens:        true,
		catalog.SurfaceGeminiEmbedContent:       true,
		catalog.SurfaceGeminiBatchEmbedContents: true,
		catalog.SurfaceGeminiInteractions:       true,

		// The video plane prices from admitted parameters, never from an
		// upstream usage object (ADR-0220).
		catalog.SurfaceVideo: false,
	}

	for _, s := range catalog.AllSurfaces() {
		if _, ok := reportsUpstreamUsage[s]; !ok {
			t.Errorf("surface %q has no entry here: decide whether it carries upstream usage, "+
				"give ParseUsage an arm for it by name, and add it to this table. "+
				"Falling through to the default arm bills the request at zero, silently.", s)
		}
	}

	// The half that catches the actual defect: a surface whose charge does not
	// come from upstream usage must not come back Present when handed a body
	// that another dialect would parse. Present short-circuits the estimate.
	openAIShapedBody := []byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":500}}`)
	for surface, reports := range reportsUpstreamUsage {
		if reports {
			continue
		}
		if got := ParseUsage(surface, openAIShapedBody); got.Present {
			t.Errorf("surface %q reported usage %+v from a body it does not own; "+
				"it has fallen through to another dialect's arm", surface, got)
		}
	}
}

// The streaming counterpart of the gate above, and it exists because the same
// sentence was owed by a second switch that nobody was holding to it.
//
// usageAccumulator.consume switches on surface just as ParseUsage does, and it
// carries the same hazard in a worse form: a streamed image's terminal
// `image_generation.completed` frame *does* carry a usage object, so the chat
// arm found one, read the two fields it knows as zero, and set Present. The
// request then settled a real generation at nothing with estimated false. The
// buffered switch had been fixed and gated; this one had not.
//
// Checking Present is therefore not enough here. Present-with-zeros is the
// failure, so every streamed surface is asserted against the actual counts its
// own frame reports.
func TestEverySurfaceIsClassifiedForStreamedUsageParsing(t *testing.T) {
	type streamedCase struct {
		// frames are consumed in order, as the pass-through would deliver them.
		frames []string
		// wantIn and wantOut are what the surface's own frames report. Zero
		// values are not accepted as a pass: they are the defect.
		wantIn, wantOut int64
		// notStreamed marks a surface no caller can put stream:true on, with
		// the reason. It is a decision recorded here, not a gap.
		notStreamed string
	}
	cases := map[catalog.Surface]streamedCase{
		catalog.SurfaceChat: {
			frames:  []string{`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":500}}`},
			wantIn:  100,
			wantOut: 500,
		},
		catalog.SurfaceMessages: {
			frames: []string{
				`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":0}}}`,
				`data: {"type":"message_delta","usage":{"output_tokens":500}}`,
			},
			wantIn:  100,
			wantOut: 500,
		},
		catalog.SurfaceResponses: {
			frames:  []string{`data: {"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":500}}}`},
			wantIn:  100,
			wantOut: 500,
		},
		catalog.SurfaceResponsesCompact: {
			frames:  []string{`data: {"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":500}}}`},
			wantIn:  100,
			wantOut: 500,
		},
		catalog.SurfaceGenerateContent: {
			frames:  []string{`data: {"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":500}}`},
			wantIn:  100,
			wantOut: 500,
		},
		catalog.SurfaceGeminiInteractions: {
			frames:  []string{`data: {"total_usage":{"total_input_tokens":100,"total_output_tokens":500}}`},
			wantIn:  100,
			wantOut: 500,
		},
		catalog.SurfaceImages: {
			// The frame that started this. partial_image events carry no usage;
			// the terminal one carries it in the input_tokens/output_tokens
			// spelling no other openai-protocol surface uses.
			frames: []string{
				`data: {"type":"image_generation.partial_image","b64_json":"aaa"}`,
				`data: {"type":"image_generation.completed","b64_json":"aaa","usage":{"input_tokens":100,"output_tokens":500}}`,
			},
			wantIn:  100,
			wantOut: 500,
		},

		// The handler forces stream off on this one: its request body is a
		// multipart upload and its response is the edited image. It shares the
		// images arm anyway rather than being left out of the switch -- if that
		// ever changes, the failure must not be the chat arm reading its usage
		// object as a row of zeros.
		catalog.SurfaceImagesEdit: {notStreamed: "multipart in, one image out; the handler forces stream off"},

		catalog.SurfaceEmbeddings:               {notStreamed: "one request, one vector set; there is no incremental form"},
		catalog.SurfaceMessagesCountTokens:      {notStreamed: "a utility endpoint that answers a count, not a generation"},
		catalog.SurfaceResponsesInputTokens:     {notStreamed: "same: a count, not a generation"},
		catalog.SurfaceResponsesResources:       {notStreamed: "reads a stored resource; nothing is generated"},
		catalog.SurfaceGeminiCountTokens:        {notStreamed: "a count, not a generation"},
		catalog.SurfaceGeminiEmbedContent:       {notStreamed: "one request, one vector"},
		catalog.SurfaceGeminiBatchEmbedContents: {notStreamed: "one request, many vectors, all at once"},
		catalog.SurfaceVideo:                    {notStreamed: "the job plane answers a job, and prices from admitted parameters (ADR-0220)"},
	}

	for _, s := range catalog.AllSurfaces() {
		if _, ok := cases[s]; !ok {
			t.Errorf("surface %q has no entry here: decide whether it can be streamed, "+
				"give usageAccumulator.consume an arm for it by name, and add it to this "+
				"table. Falling through to the default arm does not leave a streamed "+
				"request unparsed -- it parses it with the wrong arm and bills zero.", s)
		}
	}

	for surface, tc := range cases {
		if tc.notStreamed != "" {
			continue
		}
		var acc usageAccumulator
		var textBuf bytes.Buffer
		for _, frame := range tc.frames {
			acc.consume(surface, []byte(frame), &textBuf)
		}
		got := acc.result()
		switch {
		case !got.Present:
			t.Errorf("surface %q: its own frames reported no usage at all", surface)
		case got.In != tc.wantIn || got.Out != tc.wantOut:
			t.Errorf("surface %q: streamed usage in=%d out=%d, want %d/%d. "+
				"Zeros here mean the frames were read by another surface's arm.",
				surface, got.In, got.Out, tc.wantIn, tc.wantOut)
		}
	}
}
