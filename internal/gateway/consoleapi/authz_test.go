package gwconsoleapi_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	gwconsoleapi "github.com/fairlb/fairlb/internal/gateway/consoleapi"
)

// Which of the three gates each endpoint goes through: ordinary read, sensitive
// configuration read, and write. A suspended org may still read; writes get a
// 403.
//
// This file is a dispatch table, not a test of authorization logic. The actual
// role and org-status decisions live in the organization layer, which is the only
// place that knows about membership; here we pin down only which gate each of
// the gateway's endpoints uses. The reason to test the two separately: the
// gateway cannot possibly discover on its own that "a member can create a BYOK
// credential", because it has no idea what a member is.
//
// What it looked like before: there was one read authorization here and every
// endpoint used it. That was wrong in both directions at once -- the three BYOK
// write endpoints escaped the role gate, so a member could add or remove
// upstream billing credentials, contradicting what the console UI itself
// allows; and seven read endpoints were caught by the org-status gate, so a
// suspended org could not read its own usage or logs.
func TestEndpointGateDispatch(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Seed a real credential through a fully permissive server first, so that
	// a write endpoint missing its gate would really delete or test it rather
	// than failing with "not found". The reason for the failure has to be the
	// gate, not luck.
	seeded := byokServer(t, f)
	keyID := seedBYOK(t, f, seeded, "openai", "")

	// readOnlyAuthz permits ordinary and admin reads, and refuses writes.
	s := gwconsoleapi.NewServer(gwconsoleapi.ServerConfig{
		Pool: f.pool, OrganizationAccess: readOnlyAuthz{}, Catalog: testCatalog(f.pool), Cipher: byokBox(t),
	})
	org := orgParam(f.orgA)
	from, to := dayAgo(1), time.Now().Add(time.Hour)

	reads := map[string]func() error{
		"GetUsage": func() error {
			_, err := s.GetUsage(ctx, gwconsoleapi.GetUsageRequestObject{
				OrgId:  org,
				Params: gwconsoleapi.GetUsageParams{From: from, To: to},
			})
			return err
		},
		"ExportUsageCSV": func() error {
			_, err := s.ExportUsageCSV(ctx, gwconsoleapi.ExportUsageCSVRequestObject{
				OrgId:  org,
				Params: gwconsoleapi.ExportUsageCSVParams{From: from, To: to},
			})
			return err
		},
		"ListAvailableModels": func() error {
			_, err := s.ListAvailableModels(ctx, gwconsoleapi.ListAvailableModelsRequestObject{
				OrgId: org,
			})
			return err
		},
		"ListRequestLogs": func() error {
			_, err := s.ListRequestLogs(ctx, gwconsoleapi.ListRequestLogsRequestObject{
				OrgId: org,
			})
			return err
		},
		"ExportLogsCSV": func() error {
			_, err := s.ExportLogsCSV(ctx, gwconsoleapi.ExportLogsCSVRequestObject{
				OrgId: org,
			})
			return err
		},
		"ListOrgProviderKeys": func() error {
			_, err := s.ListOrgProviderKeys(ctx, gwconsoleapi.ListOrgProviderKeysRequestObject{
				OrgId: org,
			})
			return err
		},
	}
	for name, call := range reads {
		t.Run("read/"+name, func(t *testing.T) {
			if err := call(); err != nil {
				t.Errorf("%s is a read endpoint and should pass the read gate, but failed: %v"+
					"\n(if this goes red because of the write gate, it was wired to the write path -- "+
					"the consequence is that a suspended org cannot read its own data)", name, err)
			}
		})
	}

	writes := map[string]func() error{
		"CreateOrgProviderKey": func() error {
			_, err := s.CreateOrgProviderKey(ctx, gwconsoleapi.CreateOrgProviderKeyRequestObject{
				OrgId: org,
				Body: &gwconsoleapi.CreateOrgProviderKeyJSONRequestBody{
					Vendor: "openai", Name: "gate-probe", Secret: byokSecret,
				},
			})
			return err
		},
		"DeleteOrgProviderKey": func() error {
			_, err := s.DeleteOrgProviderKey(ctx, gwconsoleapi.DeleteOrgProviderKeyRequestObject{
				OrgId: org, KeyId: keyID,
			})
			return err
		},
		// The connectivity test is a write too: it sends a real request
		// upstream, which costs money, and writes the result back.
		"TestOrgProviderKey": func() error {
			_, err := s.TestOrgProviderKey(ctx, gwconsoleapi.TestOrgProviderKeyRequestObject{
				OrgId: org, KeyId: keyID,
				Body: &gwconsoleapi.TestOrgProviderKeyJSONRequestBody{UpstreamModel: "m"},
			})
			return err
		},
	}
	for name, call := range writes {
		t.Run("write/"+name, func(t *testing.T) {
			// It has to be the write gate's sentinel, not just any error:
			// see the ErrWriteDenied comment -- asserting only err != nil
			// lets the connectivity test pass for an unrelated reason.
			if err := call(); !errors.Is(err, ErrWriteDenied) {
				t.Errorf("%s is a write endpoint and must be stopped by the write gate, got err=%v"+
					"\n(before the fix a plain member could add, delete and test upstream billing credentials; "+
					"a non-nil error that is not the sentinel means it bypassed the write gate and failed deeper in)",
					name, err)
			}
		})
	}

	// After the write gate refuses, the credential must still be there. That
	// proves the three failures above were stopped before touching data, not
	// reported after doing the work.
	listed, err := s.ListOrgProviderKeys(ctx, gwconsoleapi.ListOrgProviderKeysRequestObject{
		OrgId: org,
	})
	if err != nil {
		t.Fatal(err)
	}
	keys := listed.(gwconsoleapi.ListOrgProviderKeys200JSONResponse).Items
	if len(keys) != 1 || keys[0].Id != keyID {
		t.Fatalf("after the write gate refuses, the original credential should be untouched, got %d: %+v", len(keys), keys)
	}
}

// poolBackedFinanceAuthz mimics the production authorizer: resolving the read
// projection reads member roles from the same connection pool the gateway uses.
// The trivial SELECT does not reproduce the real authorization logic, only the
// connection-holding behaviour this probe exists to pin down; role and
// credential semantics are covered by that layer's own tests.
type poolBackedFinanceAuthz struct {
	allowAll
	pool *pgxpool.Pool
}

func (a poolBackedFinanceAuthz) ResolveOrgReadAccess(ctx context.Context, _ pgtype.UUID) (bool, bool, error) {
	var allowed bool
	err := a.pool.QueryRow(ctx, `SELECT true`).Scan(&allowed)
	return allowed, allowed, err
}

// The finance and key-metadata projections must be resolved once, before
// entering the org-scoped transaction. In production a cache-invalidation
// LISTEN holds one connection permanently; if the handler then takes a second
// for its transaction and the authorizer acquires from the same pool, a single
// request deadlocks at max_conns=2. The org currency in the usage totals must
// likewise reuse the current transaction rather than going back to the pool.
func TestReadProjectionAuthorizationDoesNotReacquireInsideOrgTx(t *testing.T) {
	f := newFixture(t)
	f.log(t, f.orgA, logRow{requestID: "pool-probe", at: time.Now().Add(-time.Minute)})

	ctx := context.Background()
	cfg := f.pool.Config()
	cfg.MaxConns = 2
	cfg.MinConns = 0
	limited, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(limited.Close)

	// Simulate the permanent cache-invalidation LISTEN connection, leaving
	// exactly one working connection.
	listener, err := limited.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(listener.Release)

	s := newConsoleServer(limited, poolBackedFinanceAuthz{pool: limited})
	org := orgParam(f.orgA)
	from, to := dayAgo(1), time.Now().Add(time.Hour)
	calls := map[string]func(context.Context) error{
		"GetUsage": func(ctx context.Context) error {
			_, err := s.GetUsage(ctx, gwconsoleapi.GetUsageRequestObject{
				OrgId: org, Params: gwconsoleapi.GetUsageParams{From: from, To: to},
			})
			return err
		},
		"ExportUsageCSV": func(ctx context.Context) error {
			_, err := s.ExportUsageCSV(ctx, gwconsoleapi.ExportUsageCSVRequestObject{
				OrgId: org, Params: gwconsoleapi.ExportUsageCSVParams{From: from, To: to},
			})
			return err
		},
		"ListAvailableModels": func(ctx context.Context) error {
			_, err := s.ListAvailableModels(ctx, gwconsoleapi.ListAvailableModelsRequestObject{OrgId: org})
			return err
		},
		"ListRequestLogs": func(ctx context.Context) error {
			_, err := s.ListRequestLogs(ctx, gwconsoleapi.ListRequestLogsRequestObject{OrgId: org})
			return err
		},
		"GetRequestLog": func(ctx context.Context) error {
			_, err := s.GetRequestLog(ctx, gwconsoleapi.GetRequestLogRequestObject{
				OrgId: org, RequestId: "pool-probe",
			})
			return err
		},
		"ExportLogsCSV": func(ctx context.Context) error {
			_, err := s.ExportLogsCSV(ctx, gwconsoleapi.ExportLogsCSVRequestObject{OrgId: org})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			callCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := call(callCtx); err != nil {
				t.Fatalf("with max_conns=2 and a LISTEN holding one, it must not acquire a second connection: %v", err)
			}
		})
	}
}
