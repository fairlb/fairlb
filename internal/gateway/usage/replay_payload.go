package usage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/publicid"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
)

const usageReplayVersion = 1

// usageReplayPayload is the durable, application-owned format for retrying a
// delivered request. It deliberately does not serialize a generated sqlc
// parameter type: database code generation must not silently change persisted
// job data.
type usageReplayPayload struct {
	Version int               `json:"version"`
	Usage   usageReplayRecord `json:"usage"`
}

type usageReplayRecord struct {
	OrgID               string          `json:"org_id"`
	APIKeyID            string          `json:"api_key_id"`
	RequestID           string          `json:"request_id"`
	Surface             string          `json:"surface"`
	ModelSlug           string          `json:"model_slug"`
	ProviderID          string          `json:"provider_id"`
	ProviderKeyID       *string         `json:"provider_key_id"`
	OrgProviderKeyID    *string         `json:"org_provider_key_id"`
	BYOK                bool            `json:"byok"`
	RouteAttempts       int32           `json:"route_attempts"`
	Stream              bool            `json:"stream"`
	Status              string          `json:"status"`
	ErrorCode           string          `json:"error_code"`
	HTTPStatus          int32           `json:"http_status"`
	TokensIn            int32           `json:"tokens_in"`
	TokensOut           int32           `json:"tokens_out"`
	TokensCachedRead    int32           `json:"tokens_cached_read"`
	TokensCacheWrite    int32           `json:"tokens_cache_write"`
	TokensReasoning     int32           `json:"tokens_reasoning"`
	UsageEstimated      bool            `json:"usage_estimated"`
	UpstreamCostUSDNano int64           `json:"upstream_cost_usd_nano"`
	ChargedNano         int64           `json:"charged_nano"`
	ChargedCurrency     string          `json:"charged_currency"`
	FXRate              string          `json:"fx_rate"`
	HoldID              *string         `json:"hold_id"`
	EndUserID           string          `json:"end_user_id"`
	TTFTMs              int32           `json:"ttft_ms"`
	DurationMs          int32           `json:"duration_ms"`
	ToolCalls           json.RawMessage `json:"tool_calls"`
	ServiceTier         string          `json:"service_tier"`
	TokensAudioIn       int32           `json:"tokens_audio_in"`
	TokensAudioOut      int32           `json:"tokens_audio_out"`
	TokensImageIn       int32           `json:"tokens_image_in"`
	BilledUnits         int32           `json:"billed_units"`
	BilledUnit          string          `json:"billed_unit"`
	TokensCacheWrite5m  int32           `json:"tokens_cache_write_5m"`
	TokensCacheWrite1h  int32           `json:"tokens_cache_write_1h"`
	PricingSnapshot     json.RawMessage `json:"pricing_snapshot"`
	RouteID             *string         `json:"route_id"`
	Attempts            json.RawMessage `json:"attempts"`
}

var usageReplayFields = []string{
	"org_id", "api_key_id", "request_id", "surface", "model_slug", "provider_id",
	"provider_key_id", "org_provider_key_id", "byok", "route_attempts", "stream",
	"status", "error_code", "http_status", "tokens_in", "tokens_out",
	"tokens_cached_read", "tokens_cache_write", "tokens_reasoning", "usage_estimated",
	"upstream_cost_usd_nano", "charged_nano", "charged_currency", "fx_rate", "hold_id",
	"end_user_id", "ttft_ms", "duration_ms", "tool_calls", "service_tier",
	"tokens_audio_in", "tokens_audio_out", "tokens_image_in",
	"tokens_cache_write_5m", "tokens_cache_write_1h", "pricing_snapshot",
	"route_id", "attempts",
}

// EncodeUsageReplayPayload converts the current SQL insert shape into the one
// durable replay format accepted by this build.
func EncodeUsageReplayPayload(params gwdb.InsertUsageLogParams) ([]byte, error) {
	fxRate, err := numericText(params.FxRate)
	if err != nil {
		return nil, fmt.Errorf("usage replay: fx_rate: %w", err)
	}
	record := usageReplayRecord{
		OrgID: uuidStr(params.OrgID), APIKeyID: uuidStr(params.ApiKeyID),
		RequestID: params.RequestID, Surface: params.Surface, ModelSlug: params.ModelSlug,
		ProviderID: uuidStr(params.ProviderID), ProviderKeyID: optionalUUIDText(params.ProviderKeyID),
		OrgProviderKeyID: optionalUUIDText(params.OrgProviderKeyID), BYOK: params.Byok,
		RouteAttempts: params.RouteAttempts, Stream: params.Stream, Status: params.Status,
		ErrorCode: params.ErrorCode, HTTPStatus: params.HttpStatus,
		TokensIn: params.TokensIn, TokensOut: params.TokensOut,
		TokensCachedRead: params.TokensCachedRead, TokensCacheWrite: params.TokensCacheWrite,
		TokensReasoning: params.TokensReasoning, UsageEstimated: params.UsageEstimated,
		UpstreamCostUSDNano: params.UpstreamCostUsdNano, ChargedNano: params.ChargedNano,
		ChargedCurrency: params.ChargedCurrency, FXRate: fxRate,
		HoldID: optionalUUIDText(params.HoldID), EndUserID: params.EndUserID,
		TTFTMs: params.TtftMs, DurationMs: params.DurationMs,
		ToolCalls: cloneJSON(params.ToolCalls), ServiceTier: params.ServiceTier.String,
		TokensAudioIn: params.TokensAudioIn.Int32, TokensAudioOut: params.TokensAudioOut.Int32,
		TokensImageIn:      params.TokensImageIn.Int32,
		BilledUnits:        params.BilledUnits.Int32,
		BilledUnit:         params.BilledUnit,
		TokensCacheWrite5m: params.TokensCacheWrite5m.Int32,
		TokensCacheWrite1h: params.TokensCacheWrite1h.Int32,
		PricingSnapshot:    cloneJSON(params.PricingSnapshot),
		RouteID:            optionalUUIDText(params.RouteID), Attempts: cloneJSON(params.Attempts),
	}
	if !params.TokensAudioIn.Valid || !params.TokensAudioOut.Valid ||
		!params.TokensImageIn.Valid ||
		!params.TokensCacheWrite5m.Valid || !params.TokensCacheWrite1h.Valid {
		return nil, fmt.Errorf("usage replay: token dimensions must be present")
	}
	if err := record.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(usageReplayPayload{Version: usageReplayVersion, Usage: record})
}

// DecodeUsageReplayPayload accepts exactly version 1 with no unknown or
// missing fields. Invalid persisted work is terminal and must be abandoned and
// alerted by the caller rather than retried with guessed defaults.
func DecodeUsageReplayPayload(data []byte) (gwdb.InsertUsageLogParams, error) {
	var envelope struct {
		Version int             `json:"version"`
		Usage   json.RawMessage `json:"usage"`
	}
	if err := decodeStrict(data, &envelope); err != nil {
		return gwdb.InsertUsageLogParams{}, fmt.Errorf("usage replay: %w", err)
	}
	if envelope.Version != usageReplayVersion {
		return gwdb.InsertUsageLogParams{}, fmt.Errorf("usage replay: unsupported version %d", envelope.Version)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Usage, &fields); err != nil {
		return gwdb.InsertUsageLogParams{}, fmt.Errorf("usage replay: usage must be an object: %w", err)
	}
	for _, name := range usageReplayFields {
		if _, ok := fields[name]; !ok {
			return gwdb.InsertUsageLogParams{}, fmt.Errorf("usage replay: missing required field %q", name)
		}
	}
	var record usageReplayRecord
	if err := decodeStrict(envelope.Usage, &record); err != nil {
		return gwdb.InsertUsageLogParams{}, fmt.Errorf("usage replay: %w", err)
	}
	if err := record.validate(); err != nil {
		return gwdb.InsertUsageLogParams{}, err
	}
	return record.params()
}

func (r usageReplayRecord) validate() error {
	for name, value := range map[string]string{
		"org_id": r.OrgID, "api_key_id": r.APIKeyID, "request_id": r.RequestID,
		"surface": r.Surface, "model_slug": r.ModelSlug, "provider_id": r.ProviderID,
		"status": r.Status, "charged_currency": r.ChargedCurrency, "fx_rate": r.FXRate,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("usage replay: %s must not be empty", name)
		}
	}
	if r.RouteAttempts < 1 {
		return fmt.Errorf("usage replay: route_attempts must be positive")
	}
	if err := requireJSONType("tool_calls", r.ToolCalls, '{'); err != nil {
		return err
	}
	if err := requireJSONType("attempts", r.Attempts, '['); err != nil {
		return err
	}
	if err := requireJSONType("pricing_snapshot", r.PricingSnapshot, '{'); err != nil {
		return err
	}
	var snapshot struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(r.PricingSnapshot, &snapshot); err != nil || snapshot.SchemaVersion != 1 {
		return fmt.Errorf("usage replay: pricing_snapshot must use schema_version 1")
	}
	return nil
}

func (r usageReplayRecord) params() (gwdb.InsertUsageLogParams, error) {
	orgID, err := parseReplayUUID(r.OrgID)
	if err != nil {
		return gwdb.InsertUsageLogParams{}, fmt.Errorf("usage replay: org_id: %w", err)
	}
	apiKeyID, err := parseReplayUUID(r.APIKeyID)
	if err != nil {
		return gwdb.InsertUsageLogParams{}, fmt.Errorf("usage replay: api_key_id: %w", err)
	}
	providerID, err := parseReplayUUID(r.ProviderID)
	if err != nil {
		return gwdb.InsertUsageLogParams{}, fmt.Errorf("usage replay: provider_id: %w", err)
	}
	fxRate := pgtype.Numeric{}
	if err := fxRate.Scan(r.FXRate); err != nil {
		return gwdb.InsertUsageLogParams{}, fmt.Errorf("usage replay: fx_rate: %w", err)
	}
	providerKeyID, err := parseOptionalUUID(r.ProviderKeyID)
	if err != nil {
		return gwdb.InsertUsageLogParams{}, fmt.Errorf("usage replay: provider_key_id: %w", err)
	}
	orgProviderKeyID, err := parseOptionalUUID(r.OrgProviderKeyID)
	if err != nil {
		return gwdb.InsertUsageLogParams{}, fmt.Errorf("usage replay: org_provider_key_id: %w", err)
	}
	holdID, err := parseOptionalUUID(r.HoldID)
	if err != nil {
		return gwdb.InsertUsageLogParams{}, fmt.Errorf("usage replay: hold_id: %w", err)
	}
	routeID, err := parseOptionalUUID(r.RouteID)
	if err != nil {
		return gwdb.InsertUsageLogParams{}, fmt.Errorf("usage replay: route_id: %w", err)
	}
	return gwdb.InsertUsageLogParams{
		OrgID: orgID, ApiKeyID: apiKeyID, RequestID: r.RequestID, Surface: r.Surface,
		ModelSlug: r.ModelSlug, ProviderID: providerID, ProviderKeyID: providerKeyID,
		OrgProviderKeyID: orgProviderKeyID, Byok: r.BYOK, RouteAttempts: r.RouteAttempts,
		Stream: r.Stream, Status: r.Status, ErrorCode: r.ErrorCode, HttpStatus: r.HTTPStatus,
		TokensIn: r.TokensIn, TokensOut: r.TokensOut, TokensCachedRead: r.TokensCachedRead,
		TokensCacheWrite: r.TokensCacheWrite, TokensReasoning: r.TokensReasoning,
		UsageEstimated: r.UsageEstimated, UpstreamCostUsdNano: r.UpstreamCostUSDNano,
		ChargedNano: r.ChargedNano, ChargedCurrency: r.ChargedCurrency, FxRate: fxRate,
		HoldID: holdID, EndUserID: r.EndUserID, TtftMs: r.TTFTMs, DurationMs: r.DurationMs,
		ToolCalls: cloneJSON(r.ToolCalls), ServiceTier: pgtype.Text{String: r.ServiceTier, Valid: true},
		TokensAudioIn:      pgtype.Int4{Int32: r.TokensAudioIn, Valid: true},
		TokensAudioOut:     pgtype.Int4{Int32: r.TokensAudioOut, Valid: true},
		TokensImageIn:      pgtype.Int4{Int32: r.TokensImageIn, Valid: true},
		BilledUnits:        pgtype.Int4{Int32: r.BilledUnits, Valid: r.BilledUnit != ""},
		BilledUnit:         r.BilledUnit,
		TokensCacheWrite5m: pgtype.Int4{Int32: r.TokensCacheWrite5m, Valid: true},
		TokensCacheWrite1h: pgtype.Int4{Int32: r.TokensCacheWrite1h, Valid: true},
		PricingSnapshot:    cloneJSON(r.PricingSnapshot), RouteID: routeID,
		Attempts: cloneJSON(r.Attempts),
	}, nil
}

func decodeStrict(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func requireJSONType(name string, raw json.RawMessage, first byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != first || !json.Valid(trimmed) {
		return fmt.Errorf("usage replay: %s has invalid JSON shape", name)
	}
	return nil
}

func numericText(n pgtype.Numeric) (string, error) {
	value, err := n.Value()
	if err != nil {
		return "", err
	}
	text, ok := value.(string)
	if !ok || text == "" {
		return "", fmt.Errorf("value is missing")
	}
	return text, nil
}

var uuidStr = publicid.UUIDString

func optionalUUIDText(id pgtype.UUID) *string {
	if !id.Valid {
		return nil
	}
	value := id.String()
	return &value
}

func parseReplayUUID(value string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if err := id.Scan(value); err != nil {
		return pgtype.UUID{}, err
	}
	return id, nil
}

func parseOptionalUUID(value *string) (pgtype.UUID, error) {
	if value == nil {
		return pgtype.UUID{}, nil
	}
	return parseReplayUUID(*value)
}

func cloneJSON(raw []byte) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}
