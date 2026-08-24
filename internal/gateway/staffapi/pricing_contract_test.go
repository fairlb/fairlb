package gwstaffapi

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPricingBoundsMatchDatabaseAndOpenAPI(t *testing.T) {
	t.Parallel()
	if pricingMultiplierMax != 100_000 {
		t.Fatalf("application multiplier maximum = %d, want 100000", pricingMultiplierMax)
	}
	if maxConfigurableRateNano != 92_233_720_368_547_758 {
		t.Fatalf("application rate maximum = %d, want MaxInt64/100", maxConfigurableRateNano)
	}

	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	repo := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", ".."))
	read := func(path string) string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(repo, path))
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}

	sql := read("migrations/0002_gateway.sql")
	for _, check := range []string{
		"cost_multiplier_bps BETWEEN 1 AND 100000",
		"multiplier_bps BETWEEN 1 AND 100000",
		"nano_per_mtok BETWEEN 0 AND 92233720368547758",
	} {
		if !strings.Contains(sql, check) {
			t.Errorf("database contract is missing %q", check)
		}
	}

	openapi := read("api/gateway-staff.yaml")
	if count := strings.Count(openapi, "maximum: 100000"); count != 3 {
		t.Errorf("OpenAPI multiplier maximum occurs %d times, want 3", count)
	}
	apiRateMax := FormatNanoUSDPerM(maxConfigurableRateNano)
	if !strings.Contains(openapi, apiRateMax) {
		t.Errorf("OpenAPI rate maximum is missing %s", apiRateMax)
	}
}
