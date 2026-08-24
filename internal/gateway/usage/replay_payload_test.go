package usage

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

func TestUsageReplayPayloadAcceptsOnlyTheCurrentCompleteShape(t *testing.T) {
	encoded, err := EncodeUsageReplayPayload(replayParams(t))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeUsageReplayPayload(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.RequestID != "req-replay" || decoded.RouteAttempts != 1 || decoded.ChargedNano != 42 {
		t.Fatalf("round trip changed durable data: %+v", decoded)
	}

	cases := map[string]func(map[string]any, map[string]any){
		"missing field":   func(_ map[string]any, usage map[string]any) { delete(usage, "attempts") },
		"unknown field":   func(_ map[string]any, usage map[string]any) { usage["fallback"] = true },
		"unknown version": func(envelope map[string]any, _ map[string]any) { envelope["version"] = 2 },
		"old pricing snapshot": func(_ map[string]any, usage map[string]any) {
			usage["pricing_snapshot"] = map[string]any{"schema_version": 4}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var candidate map[string]any
			if err := json.Unmarshal(encoded, &candidate); err != nil {
				t.Fatal(err)
			}
			candidateUsage := candidate["usage"].(map[string]any)
			mutate(candidate, candidateUsage)
			raw, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeUsageReplayPayload(raw); err == nil {
				t.Fatal("invalid durable payload was accepted")
			}
		})
	}
}

func replayParams(t *testing.T) gwdb.InsertUsageLogParams {
	t.Helper()
	uuid := func(value string) pgtype.UUID {
		var id pgtype.UUID
		if err := id.Scan(value); err != nil {
			t.Fatal(err)
		}
		return id
	}
	var fx pgtype.Numeric
	if err := fx.Scan("1"); err != nil {
		t.Fatal(err)
	}
	return gwdb.InsertUsageLogParams{
		OrgID:     uuid("0198f593-25d8-7f7d-a8cb-3f817fe081e0"),
		ApiKeyID:  uuid("0198f593-25d8-7f7d-a8cb-3f817fe081e1"),
		RequestID: "req-replay", Surface: "chat", ModelSlug: "openai/test",
		ProviderID:    uuid("0198f593-25d8-7f7d-a8cb-3f817fe081e2"),
		RouteAttempts: 1, Status: "ok", HttpStatus: 200,
		ChargedNano: 42, ChargedCurrency: "USD", FxRate: fx,
		ToolCalls: json.RawMessage(`{}`), ServiceTier: pgtype.Text{String: "", Valid: true},
		TokensAudioIn: pgtype.Int4{Valid: true}, TokensAudioOut: pgtype.Int4{Valid: true},
		TokensCacheWrite5m: pgtype.Int4{Valid: true}, TokensCacheWrite1h: pgtype.Int4{Valid: true},
		PricingSnapshot: json.RawMessage(`{"schema_version":1}`),
		Attempts:        json.RawMessage(`[]`),
	}
}

func TestUsageReplayPayloadDoesNotReplaceMissingJSONWithDefaults(t *testing.T) {
	params := replayParams(t)
	params.Attempts = nil
	_, err := EncodeUsageReplayPayload(params)
	if err == nil || !strings.Contains(err.Error(), "attempts") {
		t.Fatalf("missing attempts must fail explicitly, got %v", err)
	}
}
