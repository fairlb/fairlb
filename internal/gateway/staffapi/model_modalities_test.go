package gwstaffapi_test

import (
	"context"
	"testing"

	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
)

// An explicitly empty modality list means "say nothing", not "produce nothing".
//
// The API declares minItems: 1 but nothing enforces it at run time -- the
// generated server is strict about shapes, not about constraints -- so `[]`
// reaches the column. It has to read as absent there: the driver encodes a
// non-nil empty slice as `{}` rather than as NULL, so without a nullif the
// coalesce passes `{}` straight through and the cardinality CHECK refuses it.
// The caller then gets a constraint violation for a field they were entitled to
// leave alone, and the message names a database constraint rather than the
// field.
func TestEmptyOutputModalitiesLeavesTheColumnAlone(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()

	empty := gwstaffapi.OutputModalities{}
	created, err := s.CreateGatewayModel(ctx, gwstaffapi.CreateGatewayModelRequestObject{
		Body: &gwstaffapi.GatewayModelInput{
			Slug: ptrTo("openai/empty-modalities"), OutputModalities: &empty,
		},
	})
	if err != nil {
		t.Fatalf("creating a model with an empty modality list must fall back to the "+
			"column's default rather than fail: %v", err)
	}
	model := created.(gwstaffapi.CreateGatewayModel201JSONResponse)
	if len(model.OutputModalities) != 1 || model.OutputModalities[0] != "text" {
		t.Fatalf("an unstated modality should be text, got %v", model.OutputModalities)
	}

	// Now say something, so the update below has something to preserve.
	stated := gwstaffapi.OutputModalities{"text", "image"}
	if _, err := s.UpdateGatewayModel(ctx, gwstaffapi.UpdateGatewayModelRequestObject{
		ModelId: model.Id, Body: &gwstaffapi.GatewayModelInput{OutputModalities: &stated},
	}); err != nil {
		t.Fatal(err)
	}

	updated, err := s.UpdateGatewayModel(ctx, gwstaffapi.UpdateGatewayModelRequestObject{
		ModelId: model.Id, Body: &gwstaffapi.GatewayModelInput{OutputModalities: &empty},
	})
	if err != nil {
		t.Fatalf("an empty modality list on update must leave the column alone: %v", err)
	}
	got := updated.(gwstaffapi.UpdateGatewayModel200JSONResponse).OutputModalities
	if len(got) != 2 || got[0] != "text" || got[1] != "image" {
		t.Fatalf("the stated modalities should have survived an empty update, got %v", got)
	}
}

// A modality the column does not accept is refused with a message about the
// field, not with a constraint violation.
func TestUnknownOutputModalityIsRefusedByName(t *testing.T) {
	s, _, _ := newServer(t)
	bad := gwstaffapi.OutputModalities{"audio"}
	_, err := s.CreateGatewayModel(context.Background(), gwstaffapi.CreateGatewayModelRequestObject{
		Body: &gwstaffapi.GatewayModelInput{
			Slug: ptrTo("openai/audio-model"), OutputModalities: &bad,
		},
	})
	if err == nil {
		t.Fatal("a modality outside the column's CHECK must be refused")
	}
}
