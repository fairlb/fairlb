package proxy_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/fairlb/fairlb/access/apikeys"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	"github.com/fairlb/fairlb/internal/gateway/proxy"
)

func TestUtilityOperationIsAuditedAndNeverTouchesSettlement(t *testing.T) {
	f := newPipeFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":42}`))
	})
	ctx := context.Background()
	plaintext, _, orgID := f.seedKey(t, apikeys.CreateInput{})
	f.seedCatalog(t, "anthropic", "anthropic/count", "claude-upstream", []string{"messages_count_tokens"})

	res, gerr := f.pipeline.RunUtility(ctx, proxy.Request{
		Surface: catalog.SurfaceMessagesCountTokens, Protocol: proxy.ProtocolAnthropic,
		UpstreamPath: catalog.PathMessagesCountTokens,
		Body:         []byte(`{"model":"anthropic/count","messages":[{"role":"user","content":"hi"}]}`),
		Credential:   plaintext, RequestID: "utility-no-charge",
	})
	if gerr != nil || res.Status != http.StatusOK {
		t.Fatalf("utility request failed: status=%d error=%v", res.Status, gerr)
	}
	if holds, voids, settles := f.settler.Counts(); holds != 0 || voids != 0 || settles != 0 {
		t.Fatalf("utility request touched monetary settlement: holds=%d voids=%d settles=%d", holds, voids, settles)
	}

	var tokensIn, tokensOut int32
	var charged int64
	var utility bool
	if err := f.pool.QueryRow(ctx, `
		SELECT tokens_in, tokens_out, charged_nano,
		       COALESCE((pricing_snapshot->>'utility')::boolean, false)
		  FROM usage_logs
		 WHERE org_id = $1 AND request_id = 'utility-no-charge'`, orgID,
	).Scan(&tokensIn, &tokensOut, &charged, &utility); err != nil {
		t.Fatal(err)
	}
	if tokensIn != 42 || tokensOut != 0 || charged != 0 || !utility {
		t.Fatalf("utility audit row = input:%d output:%d charged:%d utility:%v", tokensIn, tokensOut, charged, utility)
	}
}
