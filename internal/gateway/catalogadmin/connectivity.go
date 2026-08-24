package catalogadmin

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/internal/gateway/catalog"
	gwdb "github.com/fairlb/fairlb/internal/gateway/db"
	"github.com/fairlb/fairlb/internal/gateway/routeprobe"
	"github.com/fairlb/fairlb/internal/gateway/upstreamprobe"
)

// The connectivity test: one minimal but real request upstream.
//
// Both success and failure are results, not errors. A failed probe is what the
// test found, and expressing it as a failure would force every caller to
// distinguish "the probe ran and did not pass" from "the probe itself is
// broken" -- two things that call for completely different responses.

// ConnectivityRequest is what to probe.
type ConnectivityRequest struct {
	UpstreamModel string
	// Endpoint may be empty on a single-dialect provider, which then uses that
	// dialect's canonical endpoint. A multi-dialect provider has no single
	// canonical endpoint, and guessing wrong sends the operator off to
	// investigate a provider outage that never happened.
	Endpoint string
	// KeyID may be zero, which takes the first usable key in id order -- the
	// oldest one, matching how the router picks.
	//
	// It exists because otherwise a standby key can never be verified: you add
	// a new key during rotation, click test, and the probe still exercises the
	// old one, leaving the new key's last-verified stuck at "never" -- and that
	// is exactly the key expected to take over when the primary trips.
	KeyID uuid.UUID
	// WantTrace is the caller's half of the trace decision; the deployment's
	// half is checked before this is ever set.
	WantTrace bool
}

// TestConnectivity probes one provider and returns what happened.
//
// It does not record the verification: the caller may still amend the message
// (a discarded trace says so), and a record written before that amendment would
// name a result nobody can reproduce. RecordKeyVerification is the second half.
func (s *Service) TestConnectivity(
	ctx context.Context, box *crypto.Box, hc *http.Client,
	providerID uuid.UUID, in ConnectivityRequest,
) (routeprobe.Result, error) {
	model := strings.TrimSpace(in.UpstreamModel)
	if model == "" {
		return routeprobe.Result{}, invalid("upstream_model is required")
	}
	prov, err := s.q.GetProviderForAdmin(ctx, pgID(providerID))
	if err != nil {
		return routeprobe.Result{}, ErrNotFound
	}

	endpoint, err := probeEndpointFor(prov.Protocols, in.Endpoint)
	if err != nil {
		return routeprobe.Result{}, err
	}

	keyID, secretEnc, refusal, err := s.credentialFor(ctx, providerID, in.KeyID)
	if err != nil {
		return routeprobe.Result{}, err
	}
	if refusal != nil {
		return *refusal, nil
	}
	plain, err := box.Open(secretEnc, keyID.Bytes[:])
	if err != nil {
		return routeprobe.Result{
			CheckedAt: time.Now().UTC(), KeyID: uuid.UUID(keyID.Bytes),
			Message: "Could not decrypt the credential. Has the master encryption key changed?",
		}, nil
	}

	return routeprobe.Run(ctx, hc, routeprobe.Target{
		BaseURL: prov.BaseUrl, Protocols: prov.Protocols,
		Headers: prov.Headers, Transport: prov.Transport,
	}, string(plain), model, endpoint, uuid.UUID(keyID.Bytes), in.WantTrace), nil
}

// probeEndpointFor decides which endpoint to probe.
func probeEndpointFor(providerProtocols []string, requested string) (string, error) {
	if requested != "" {
		epProtocol, ok := catalog.ProtocolForEndpoint(requested)
		if !ok || !slices.Contains(providerProtocols, epProtocol) {
			return "", invalid(
				"Endpoint %s belongs to the %s protocol, but this provider only "+
					"declares %s. Probing it would fail for a reason that says "+
					"nothing about the provider.",
				requested, epProtocol, strings.Join(providerProtocols, ", "))
		}
		return requested, nil
	}
	if len(providerProtocols) != 1 {
		return "", invalid(
			"This provider declares more than one protocol (%s); name the endpoint to probe.",
			strings.Join(providerProtocols, ", "))
	}
	return upstreamprobe.DefaultEndpoint(providerProtocols[0]), nil
}

// credentialFor picks the credential to probe with.
//
// A non-nil refusal is a *result* to return as-is: "this key is disabled" and
// "there is no key" are findings of the test, not failures of it.
func (s *Service) credentialFor(
	ctx context.Context, providerID, requested uuid.UUID,
) (pgtype.UUID, []byte, *routeprobe.Result, error) {
	if requested != (uuid.UUID{}) {
		// Ownership must be checked. Without it, passing another provider's
		// key_id would send that credential to this provider's base_url. The
		// query carries a provider_id condition, so a cross-provider id matches
		// zero rows.
		row, err := s.q.GetProviderKeyForProvider(ctx, gwdb.GetProviderKeyForProviderParams{
			ID: pgID(requested), ProviderID: pgID(providerID),
		})
		if err != nil {
			return pgtype.UUID{}, nil, nil, InvalidError{
				Message: "key_id does not belong to this provider"}
		}
		if row.Status != "active" {
			return pgtype.UUID{}, nil, &routeprobe.Result{
				CheckedAt: time.Now().UTC(), KeyID: uuid.UUID(row.ID.Bytes),
				Message: "This credential is disabled. Enable it and test again, or pick another one.",
			}, nil
		}
		return row.ID, row.SecretEnc, nil, nil
	}
	keys, err := s.q.GetProviderKeysForProvider(ctx, pgID(providerID))
	if err != nil || len(keys) == 0 {
		return pgtype.UUID{}, nil, &routeprobe.Result{
			CheckedAt: time.Now().UTC(), Message: "This provider has no usable credential",
		}, nil
	}
	return keys[0].ID, keys[0].SecretEnc, nil, nil
}

// RecordKeyVerification stores the outcome so the list view can show the last
// verification without anyone paying for another probe.
//
// Failing to record it does not change the result of the test itself, which is
// why this returns nothing: there is no decision for the caller to make.
func (s *Service) RecordKeyVerification(ctx context.Context, keyID uuid.UUID, message string) {
	_ = s.q.MarkProviderKeyVerified(ctx, gwdb.MarkProviderKeyVerifiedParams{
		ID: pgID(keyID), LastError: message,
	})
}
