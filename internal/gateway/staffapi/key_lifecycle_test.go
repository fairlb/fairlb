package gwstaffapi_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	gwstaffapi "github.com/fairlb/fairlb/internal/gateway/staffapi"
)

// The credential surface. Three properties, each answering a question the
// operator page otherwise cannot:
//
//  1. Can this key be used right now? Per-key cooldown is recorded in
//     cooldowns(scope='provider_key'), while the page shows only status and
//     last-verified. Routing can therefore have dropped a key while the page
//     still calls it active.
//  2. How do you disable a key without deleting it? With only create and
//     delete, rotation has to start by removing the old one.
//  3. How do you verify a new key? A probe that always takes the first key in
//     id order can never exercise a newly added one -- and that is precisely
//     the key expected to take over when the primary trips.

func newProviderWithKeys(t *testing.T, s *gwstaffapi.Server, slug string, names ...string) (
	uuid.UUID, []uuid.UUID,
) {
	t.Helper()
	ctx := context.Background()
	created, err := s.CreateGatewayProvider(ctx, gwstaffapi.CreateGatewayProviderRequestObject{
		Body: &gwstaffapi.GatewayProviderInput{
			Slug:      ptr(slug),
			Vendor:    &vendorCustom,
			Protocols: &[]gwstaffapi.GatewayProviderInputProtocols{"openai"},
			BaseUrl:   ptr("https://upstream.test/"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	providerID := uuid.UUID(created.(gwstaffapi.CreateGatewayProvider201JSONResponse).Id)

	keys := make([]uuid.UUID, 0, len(names))
	for _, name := range names {
		// Space the keys apart in time. The primary key is a UUIDv7 and key
		// selection (the probe included) takes the first in id order, which is
		// the oldest. Creating them within the same millisecond turns "which
		// one is first" into a coin flip, and what these tests assert is
		// exactly "omit it and you get the oldest, name it and you get the one
		// you named".
		time.Sleep(2 * time.Millisecond)
		out, keyErr := s.CreateGatewayProviderKey(ctx, gwstaffapi.CreateGatewayProviderKeyRequestObject{
			ProviderId: providerID,
			Body: &gwstaffapi.CreateGatewayProviderKeyJSONRequestBody{
				Name: ptr(name), Secret: "sk-test-" + name,
			},
		})
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		keys = append(keys, uuid.UUID(out.(gwstaffapi.CreateGatewayProviderKey201JSONResponse).Id))
	}
	return providerID, keys
}

func ptr[T any](v T) *T { return &v }

func listKeys(t *testing.T, s *gwstaffapi.Server, providerID uuid.UUID) []gwstaffapi.GatewayProviderKey {
	t.Helper()
	out, err := s.ListGatewayProviderKeys(context.Background(),
		gwstaffapi.ListGatewayProviderKeysRequestObject{ProviderId: providerID})
	if err != nil {
		t.Fatal(err)
	}
	return out.(gwstaffapi.ListGatewayProviderKeys200JSONResponse).Items
}

// "Can this key be used" on the operator page must come from the same place
// routing reads it from.
func TestKeyListSurfacesTheCooldownRoutingActuallyUses(t *testing.T) {
	s, pool, _ := newServer(t)
	ctx := context.Background()
	providerID, keys := newProviderWithKeys(t, s, "cooldown/p", "k1")

	// Positive control: with no cooldown recorded the column is empty --
	// otherwise the assertion below could be vacuously true.
	if before := listKeys(t, s, providerID); before[0].CooldownUntil != nil {
		t.Fatal("cooldown_until already has a value before any cooldown row was written -- the criterion cannot measure a difference")
	}

	// This is the table routing itself writes when it opens a breaker, so the
	// fixture writes the same shape.
	if _, err := pool.Exec(ctx, `
		INSERT INTO cooldowns (scope, ref_id, until, reason)
		VALUES ('provider_key', $1, now() + interval '10 minutes', 'key-level failure')`,
		keys[0]); err != nil {
		t.Fatal(err)
	}

	rows := listKeys(t, s, providerID)
	if rows[0].CooldownUntil == nil {
		t.Fatal("the key is cooling down and the operator page cannot see it -- " +
			"routing has already taken it out of rotation while the page still shows it as active")
	}

	// An expired cooldown does not count: the query requires until > now(),
	// or a key that recovered long ago would show as cooling down forever.
	if _, err := pool.Exec(ctx, `
		UPDATE cooldowns SET until = now() - interval '1 minute'
		WHERE scope = 'provider_key' AND ref_id = $1`, keys[0]); err != nil {
		t.Fatal(err)
	}
	if listKeys(t, s, providerID)[0].CooldownUntil != nil {
		t.Error("the cooldown has expired yet it still reports as cooling down")
	}
}

// Disabling is a necessary step of rotation; without it the only option is
// deletion.
func TestKeyCanBeDisabledAndReenabledWithoutDeleting(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	providerID, keys := newProviderWithKeys(t, s, "disable/p", "k1")

	disabled := gwstaffapi.UpdateGatewayProviderKeyJSONBodyStatus("disabled")
	if _, err := s.UpdateGatewayProviderKey(ctx, gwstaffapi.UpdateGatewayProviderKeyRequestObject{
		ProviderId: providerID, KeyId: keys[0],
		Body: &gwstaffapi.UpdateGatewayProviderKeyJSONRequestBody{Status: &disabled},
	}); err != nil {
		t.Fatal(err)
	}

	rows := listKeys(t, s, providerID)
	if len(rows) != 1 {
		t.Fatalf("disabling must not delete the row, %d rows left", len(rows))
	}
	if rows[0].Status != "disabled" {
		t.Errorf("status = %q, want disabled", rows[0].Status)
	}

	// A disabled key is no longer a routing candidate: the lookup requires
	// status='active'.
	routable, err := s.TestGatewayProvider(ctx, gwstaffapi.TestGatewayProviderRequestObject{
		ProviderId: providerID,
		Body:       &gwstaffapi.TestGatewayProviderJSONRequestBody{UpstreamModel: "gpt-4o"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res := routable.(gwstaffapi.TestGatewayProvider200JSONResponse); res.Ok {
		t.Error("every key is disabled yet the connectivity test reports reachable -- disabling is not reaching key selection")
	}

	active := gwstaffapi.UpdateGatewayProviderKeyJSONBodyStatus("active")
	if _, err := s.UpdateGatewayProviderKey(ctx, gwstaffapi.UpdateGatewayProviderKeyRequestObject{
		ProviderId: providerID, KeyId: keys[0],
		Body: &gwstaffapi.UpdateGatewayProviderKeyJSONRequestBody{Status: &active},
	}); err != nil {
		t.Fatal(err)
	}
	if listKeys(t, s, providerID)[0].Status != "active" {
		t.Error("re-enabling failed -- if disabling is irreversible it is just a slower delete")
	}
}

// A cross-provider key update has to fail: a tampered path parameter must not
// reach somebody else's credential.
func TestKeyUpdateRefusesToCrossProviders(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	_, keys := newProviderWithKeys(t, s, "cross/a", "k1")
	otherID, _ := newProviderWithKeys(t, s, "cross/b", "k1")

	disabled := gwstaffapi.UpdateGatewayProviderKeyJSONBodyStatus("disabled")
	if _, err := s.UpdateGatewayProviderKey(ctx, gwstaffapi.UpdateGatewayProviderKeyRequestObject{
		ProviderId: otherID, KeyId: keys[0], // A's key, passed off as B's
		Body: &gwstaffapi.UpdateGatewayProviderKeyJSONRequestBody{Status: &disabled},
	}); err == nil {
		t.Error("a key_id from another provider was updated successfully -- a write to a sub-resource must be conditioned on the parent id")
	}
}

// The connectivity probe must accept which key to use, or a standby key can
// never be verified.
func TestConnectivityTestTargetsTheRequestedKey(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	// Two keys, k1 older. With none named, the probe always uses k1 -- first
	// in id order, the same way the router picks.
	providerID, keys := newProviderWithKeys(t, s, "probe/p", "k1", "k2")
	oldKey, newKey := keys[0], keys[1]

	run := func(keyID *uuid.UUID) gwstaffapi.GatewayProviderTestResult {
		t.Helper()
		body := &gwstaffapi.TestGatewayProviderJSONRequestBody{UpstreamModel: "gpt-4o"}
		if keyID != nil {
			body.KeyId = keyID
		}
		out, err := s.TestGatewayProvider(ctx, gwstaffapi.TestGatewayProviderRequestObject{
			ProviderId: providerID, Body: body,
		})
		if err != nil {
			t.Fatal(err)
		}
		return gwstaffapi.GatewayProviderTestResult(out.(gwstaffapi.TestGatewayProvider200JSONResponse))
	}

	// The upstream is fake and the probe is bound to fail; this test does not
	// care whether it passed. What it cares about is which key was used, and
	// that comes back as key_id.
	if got := run(nil); got.KeyId == nil || uuid.UUID(*got.KeyId) != oldKey {
		t.Errorf("with no key_id the old behaviour should hold (the oldest key %v), got %v", oldKey, got.KeyId)
	}
	if got := run(&newKey); got.KeyId == nil || uuid.UUID(*got.KeyId) != newKey {
		t.Errorf("a key_id was given but a different key was probed (want %v, got %v) -- "+
			"the standby key can then never be verified, and it is exactly the one that takes over when the primary trips", newKey, got.KeyId)
	}

	// The record has to land on the row that was named, or "last verified" on
	// screen still describes the wrong key.
	rows := listKeys(t, s, providerID)
	var newRow *gwstaffapi.GatewayProviderKey
	for i := range rows {
		if uuid.UUID(rows[i].Id) == newKey {
			newRow = &rows[i]
		}
	}
	if newRow == nil || newRow.LastVerifiedAt == nil {
		t.Error("after a targeted probe that key's last_verified_at is still empty -- the record landed on a different row")
	}
}

// Probing with another provider's key_id must be refused: otherwise that
// credential can be sent to this provider's base_url.
func TestConnectivityTestRefusesForeignKeys(t *testing.T) {
	s, _, _ := newServer(t)
	ctx := context.Background()
	_, keys := newProviderWithKeys(t, s, "foreign/a", "k1")
	otherID, _ := newProviderWithKeys(t, s, "foreign/b", "k1")

	foreign := keys[0]
	if _, err := s.TestGatewayProvider(ctx, gwstaffapi.TestGatewayProviderRequestObject{
		ProviderId: otherID,
		Body: &gwstaffapi.TestGatewayProviderJSONRequestBody{
			UpstreamModel: "gpt-4o", KeyId: &foreign,
		},
	}); err == nil {
		t.Error("another provider's key successfully probed this provider -- " +
			"that amounts to sending that key in the clear to this base_url")
	}
}
