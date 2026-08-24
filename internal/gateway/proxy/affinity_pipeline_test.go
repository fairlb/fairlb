package proxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/foundation/errcode"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

func TestStatefulResponseIsOrgScopedAndDoesNotFailOverWhenItsRouteStops(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_affinity",
			"usage":{"input_tokens":3,"output_tokens":2}
		}`))
	})
	ctx := context.Background()
	ownerKey, _, ownerOrg := f.seedKey(t, apikeys.CreateInput{})
	otherKey, _, _ := f.seedKey(t, apikeys.CreateInput{})
	f.seedCatalog(t, "openai", "openai/stateful", "stateful-upstream", []string{"responses"})

	if _, gerr := f.pipeline.Run(ctx, proxy.Request{
		Surface: catalog.SurfaceResponses, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: catalog.PathResponses,
		Body:         []byte(`{"model":"openai/stateful","input":"hi"}`),
		Credential:   ownerKey, RequestID: "stateful-create",
	}); gerr != nil {
		t.Fatalf("creating the stored response failed: %v", gerr)
	}
	var affinityCount int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM resource_affinities WHERE org_id = $1 AND upstream_id = 'resp_affinity'`,
		ownerOrg,
	).Scan(&affinityCount); err != nil || affinityCount != 1 {
		t.Fatalf("stored response affinity count = %d, err=%v", affinityCount, err)
	}

	resourceRequest := func(key, requestID string) *proxy.Error {
		_, gerr := f.pipeline.RunUtility(ctx, proxy.Request{
			Surface: catalog.SurfaceResponsesResources, Protocol: proxy.ProtocolOpenAI,
			UpstreamPath: catalog.PathResponsesResource, Method: http.MethodGet,
			Resource: "resp_affinity", Model: "openai/stateful",
			Credential: key, RequestID: requestID,
		})
		return gerr
	}
	if gerr := resourceRequest(otherKey, "stateful-cross-org"); gerr == nil || gerr.Code != errcode.GatewayResourceNotFound {
		t.Fatalf("cross-org lookup error = %v, want resource_not_found", gerr)
	}

	if _, err := f.pool.Exec(ctx, `UPDATE model_routes SET enabled = false`); err != nil {
		t.Fatal(err)
	}
	if gerr := resourceRequest(ownerKey, "stateful-route-down"); gerr == nil || gerr.Code != errcode.GatewayStateRouteUnavailable {
		t.Fatalf("disabled original route error = %v, want state_route_unavailable", gerr)
	}
}

func TestResponsesCompactUsesTheRouteThatCreatedThePreviousResponse(t *testing.T) {
	var originalCompactCalls atomic.Int32
	var originalInputTokenCalls atomic.Int32
	f := newPipeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == catalog.PathResponsesCompact {
			originalCompactCalls.Add(1)
		}
		if r.URL.Path == catalog.PathResponsesInputTokens {
			originalInputTokenCalls.Add(1)
		}
		_, _ = w.Write([]byte(`{
			"id":"resp_compact_affinity",
			"usage":{"input_tokens":3,"output_tokens":2}
		}`))
	})
	var otherCalls atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":3,"output_tokens":2}}`))
	}))
	t.Cleanup(other.Close)

	f.seedCatalogAt(t, f.upstream.URL, "openai", "compact-origin", 10)
	f.seedCatalogAt(t, other.URL, "openai", "compact-other", 20)
	f.seedModelWithRoutes(t, "openai/compact-affinity", "openai", []string{"compact-origin", "compact-other"})

	key, _, _ := f.seedKey(t, apikeys.CreateInput{})
	if _, gerr := f.pipeline.Run(context.Background(), proxy.Request{
		Surface: catalog.SurfaceResponses, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: catalog.PathResponses,
		Body:         []byte(`{"model":"openai/compact-affinity","input":"hi"}`),
		Credential:   key, RequestID: "compact-create",
	}); gerr != nil {
		t.Fatalf("creating the previous response failed: %v", gerr)
	}
	if _, gerr := f.pipeline.Run(context.Background(), proxy.Request{
		Surface: catalog.SurfaceResponsesCompact, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: catalog.PathResponsesCompact,
		Body:         []byte(`{"previous_response_id":"resp_compact_affinity"}`),
		Credential:   key, RequestID: "compact-followup",
	}); gerr != nil {
		t.Fatalf("compacting the previous response failed: %v", gerr)
	}
	if _, gerr := f.pipeline.RunUtility(context.Background(), proxy.Request{
		Surface: catalog.SurfaceResponsesInputTokens, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: catalog.PathResponsesInputTokens,
		Body:         []byte(`{"previous_response_id":"resp_compact_affinity"}`),
		Credential:   key, RequestID: "input-tokens-followup",
	}); gerr != nil {
		t.Fatalf("counting the previous response input failed: %v", gerr)
	}
	if got := originalCompactCalls.Load(); got != 1 {
		t.Fatalf("original route compact calls = %d, want 1", got)
	}
	if got := originalInputTokenCalls.Load(); got != 1 {
		t.Fatalf("original route input token calls = %d, want 1", got)
	}
	if got := otherCalls.Load(); got != 0 {
		t.Fatalf("secondary route calls = %d, want 0", got)
	}
}

func TestPreviousResponseUpstream404RemainsResourceError(t *testing.T) {
	var calls atomic.Int32
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) > 1 {
			http.Error(w, `{"error":"expired"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{
			"id":"resp_followup_expired",
			"usage":{"input_tokens":3,"output_tokens":2}
		}`))
	})
	f.seedCatalog(t, "openai", "openai/expired-followup", "expired-followup-upstream", []string{"responses"})
	key, _, _ := f.seedKey(t, apikeys.CreateInput{})

	if _, gerr := f.pipeline.Run(context.Background(), proxy.Request{
		Surface: catalog.SurfaceResponses, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: catalog.PathResponses,
		Body:         []byte(`{"model":"openai/expired-followup","input":"hi"}`),
		Credential:   key, RequestID: "expired-followup-create",
	}); gerr != nil {
		t.Fatalf("creating the response failed: %v", gerr)
	}
	request := proxy.Request{
		Surface: catalog.SurfaceResponses, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: catalog.PathResponses,
		Body:         []byte(`{"previous_response_id":"resp_followup_expired","input":"next"}`),
		Credential:   key, RequestID: "expired-followup-buffered",
	}
	if _, gerr := f.pipeline.Run(context.Background(), request); gerr == nil || gerr.Code != errcode.GatewayResourceNotFound {
		t.Fatalf("buffered upstream follow-up 404 = %v, want resource_not_found", gerr)
	}

	request.Body = []byte(`{"previous_response_id":"resp_followup_expired","input":"next","stream":true}`)
	request.RequestID = "expired-followup-stream"
	recorder := httptest.NewRecorder()
	if gerr := f.pipeline.RunStream(context.Background(), recorder, request, proxy.SurfaceOpenAI); gerr == nil || gerr.Code != errcode.GatewayResourceNotFound {
		t.Fatalf("streaming upstream follow-up 404 = %v, want resource_not_found", gerr)
	}
}

func TestResourceUpstream404RemainsAResourceError(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			http.Error(w, `{"error":"expired"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{
			"id":"resp_expired_upstream",
			"usage":{"input_tokens":3,"output_tokens":2}
		}`))
	})
	f.seedCatalog(t, "openai", "openai/expired-resource", "expired-upstream", []string{"responses"})
	key, _, _ := f.seedKey(t, apikeys.CreateInput{})

	if _, gerr := f.pipeline.Run(context.Background(), proxy.Request{
		Surface: catalog.SurfaceResponses, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: catalog.PathResponses,
		Body:         []byte(`{"model":"openai/expired-resource","input":"hi"}`),
		Credential:   key, RequestID: "expired-create",
	}); gerr != nil {
		t.Fatalf("creating the response failed: %v", gerr)
	}
	_, gerr := f.pipeline.RunUtility(context.Background(), proxy.Request{
		Surface: catalog.SurfaceResponsesResources, Protocol: proxy.ProtocolOpenAI,
		UpstreamPath: catalog.PathResponsesResource, Method: http.MethodGet,
		Resource: "resp_expired_upstream", Model: "openai/expired-resource",
		Credential: key, RequestID: "expired-get",
	})
	if gerr == nil || gerr.Code != errcode.GatewayResourceNotFound {
		t.Fatalf("upstream resource 404 error = %v, want resource_not_found", gerr)
	}
}
