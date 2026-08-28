package settings_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/foundation/db"
	"github.com/fairlb/fairlb/foundation/drivers/cache"
	"github.com/fairlb/fairlb/foundation/testutil/testpg"
	"github.com/fairlb/fairlb/settings"
)

// A channel's worth of keys: two credentials, a product id and an amount, and
// a selector on a separate section that must name a configured channel.
func channelSpecs() []settings.Spec {
	return []settings.Spec{
		{Key: "t.chan.api_key", Kind: settings.KindSecret, Section: "t.chan"},
		{Key: "t.chan.webhook_secret", Kind: settings.KindSecret, Section: "t.chan"},
		{Key: "t.chan.product_id", Kind: settings.KindString, Section: "t.chan"},
		{Key: "t.chan.amount", Kind: settings.KindInt, Section: "t.chan"},
		{Key: "t.chan.currency", Kind: settings.KindString, Section: "t.chan", Default: json.RawMessage(`"USD"`)},
		{Key: "t.sell.provider", Kind: settings.KindString, Section: "t.sell", Default: json.RawMessage(`""`)},
		{Key: "t.lonely", Kind: settings.KindInt},
	}
}

func channelRules() []settings.SectionRule {
	return []settings.SectionRule{
		{Section: "t.chan", Check: settings.RequireAllOrNone("t.chan.api_key", "t.chan.product_id", "t.chan.amount")},
		{Section: "t.sell", Reads: []string{"t.chan"}, Check: func(v settings.Values) error {
			switch p := v.String("t.sell.provider"); p {
			case "":
				return nil
			case "chan":
				if !v.Set("t.chan.api_key") {
					return errors.New("selling provider chan is not configured")
				}
				return nil
			default:
				return errors.New("unknown provider " + p)
			}
		}},
	}
}

func newBox(t *testing.T) *crypto.Box {
	t.Helper()
	box, err := crypto.NewBox(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return box
}

type fixture struct {
	pool *pgxpool.Pool
	st   *settings.Store
	mem  *cache.Memory
}

func newFixture(t *testing.T, cached bool) *fixture {
	t.Helper()
	pool := testpg.Start(t)
	var c cache.Store
	var mem *cache.Memory
	if cached {
		var err error
		if mem, err = cache.NewMemory(pool, 64); err != nil {
			t.Fatal(err)
		}
		c = mem
	}
	reg := settings.NewRegistry(channelSpecs())
	reg.MustAddRules(channelRules()...)
	return &fixture{pool: pool, st: settings.New(pool, c, reg, newBox(t)), mem: mem}
}

func (f *fixture) apply(changes ...settings.Change) error {
	return db.WithSystemTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		keys, err := f.st.Apply(context.Background(), tx, changes, "test")
		if err != nil {
			return err
		}
		for _, k := range keys {
			f.st.Invalidate(context.Background(), k)
		}
		return nil
	})
}

func change(key, raw string) settings.Change {
	return settings.Change{Key: key, Value: json.RawMessage(raw)}
}

// A secret goes in, is readable by the process, and comes back out of the
// listing as a hint only. The cache entry for it is the ciphertext — the exact
// bytes stored — never the value.
func TestSecretRoundTripsAndNeverLeavesAsPlaintext(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, true)
	pool, st, mem := f.pool, f.st, f.mem
	const plain = "sk-live-0123456789abcdef"
	if err := f.apply(
		change("t.chan.api_key", `"`+plain+`"`), change("t.chan.product_id", `"prod_1"`), change("t.chan.amount", `500`)); err != nil {
		t.Fatal(err)
	}

	var got string
	if found, err := st.Get(ctx, "t.chan.api_key", &got); err != nil || !found || got != plain {
		t.Fatalf("Get = (%q, %v, %v)", got, found, err)
	}

	var sealed []byte
	if err := pool.QueryRow(ctx, `SELECT secret_enc FROM settings WHERE key = 't.chan.api_key'`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(plain)) {
		t.Fatal("the stored bytes contain the plaintext")
	}
	cached, ok, err := mem.Get(ctx, "settings:t.chan.api_key")
	if err != nil || !ok {
		t.Fatalf("the read should have populated the cache: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(cached, sealed) {
		t.Fatalf("the cache must hold the ciphertext as stored; got %d bytes vs %d", len(cached), len(sealed))
	}

	entries, err := st.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Spec.Key != "t.chan.api_key" {
			continue
		}
		if !e.Set || e.Value != nil || e.Hint == "" || strings.Contains(e.Hint, "0123456789") {
			t.Fatalf("listing of a secret = set:%v value:%s hint:%q", e.Set, e.Value, e.Hint)
		}
		if !strings.HasSuffix(e.Hint, "cdef") {
			t.Fatalf("hint should end with the tail of the secret: %q", e.Hint)
		}
	}
}

// A ciphertext moved onto another key does not open: the key name is the AAD.
func TestSecretCiphertextIsBoundToItsKey(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, false)
	if err := f.apply(change("t.chan.api_key", `"abcdefgh"`), change("t.chan.product_id", `"p"`), change("t.chan.amount", `1`)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO settings (key, secret_enc, secret_hint)
		SELECT 't.chan.webhook_secret', secret_enc, secret_hint FROM settings WHERE key = 't.chan.api_key'`); err != nil {
		t.Fatal(err)
	}
	var got string
	_, err := f.st.Get(ctx, "t.chan.webhook_secret", &got)
	if !errors.Is(err, settings.ErrSecretUnreadable) {
		t.Fatalf("a ciphertext copied from another key should not open; got err=%v value=%q", err, got)
	}
}

// Writing "" to a secret deletes the row; the key reads as unset afterwards and
// the section rule sees it gone.
func TestEmptySecretClearsTheRow(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, false)
	pool, st := f.pool, f.st
	if err := f.apply(change("t.chan.api_key", `"abcdefgh"`), change("t.chan.product_id", `"p"`), change("t.chan.amount", `1`)); err != nil {
		t.Fatal(err)
	}
	// Clearing only the secret leaves the section half configured: rejected.
	err := f.apply(change("t.chan.api_key", `""`))
	var se *settings.SectionError
	if !errors.As(err, &se) || se.Section != "t.chan" {
		t.Fatalf("clearing one key of a configured section should fail the all-or-nothing rule; got %v", err)
	}
	// Clearing the whole section is fine.
	if err := f.apply(change("t.chan.api_key", `""`), change("t.chan.product_id", `""`), change("t.chan.amount", `0`)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM settings WHERE key = 't.chan.api_key'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("an empty secret should delete the row; %d rows remain", n)
	}
	var got string
	if found, _ := st.Get(ctx, "t.chan.api_key", &got); found {
		t.Fatal("a cleared secret should read as unset")
	}
}

func TestNewPanicsWithSecretsAndNoBox(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a registry with a secret key and no Box should not build a Store")
		}
	}()
	settings.New(testpg.Start(t), nil, settings.NewRegistry(channelSpecs()), nil)
}

// Each way a batch can be wrong, and the error type that names it.
func TestApplyRejectsEachFailurePath(t *testing.T) {
	f := newFixture(t, false)
	st := f.st
	cases := []struct {
		name    string
		changes []settings.Change
		want    func(error) bool
	}{
		{"unknown key", []settings.Change{change("t.nope", `1`)}, func(err error) bool { return errors.Is(err, settings.ErrUnknownKey) }},
		{"value off its spec", []settings.Change{change("t.chan.amount", `"ten"`)}, func(err error) bool {
			var ve *settings.ValidationError
			return errors.As(err, &ve) && ve.Key == "t.chan.amount"
		}},
		{"same key twice", []settings.Change{change("t.lonely", `1`), change("t.lonely", `2`)}, func(err error) bool {
			var ve *settings.ValidationError
			return errors.As(err, &ve)
		}},
		{"half a channel", []settings.Change{change("t.chan.api_key", `"abcdefgh"`)}, func(err error) bool {
			var se *settings.SectionError
			return errors.As(err, &se) && se.Section == "t.chan"
		}},
		{"selling an unconfigured channel", []settings.Change{change("t.sell.provider", `"chan"`)}, func(err error) bool {
			var se *settings.SectionError
			return errors.As(err, &se) && se.Section == "t.sell"
		}},
		{"selling an unknown channel", []settings.Change{change("t.sell.provider", `"other"`)}, func(err error) bool {
			var se *settings.SectionError
			return errors.As(err, &se) && se.Section == "t.sell"
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := f.apply(c.changes...)
			if err == nil || !c.want(err) {
				t.Fatalf("got %v", err)
			}
		})
	}
	// Nothing from the failed batches landed.
	entries, err := st.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Set {
			t.Fatalf("%s was written by a batch that should have been rejected", e.Spec.Key)
		}
	}
}

// A rule on one section re-runs when a section it reads changes: with "chan"
// selected as the selling channel, clearing chan's credentials is refused.
func TestRuleRerunsWhenAReadSectionChanges(t *testing.T) {
	f := newFixture(t, false)
	st := f.st
	if err := f.apply(
		change("t.chan.api_key", `"abcdefgh"`), change("t.chan.product_id", `"p"`), change("t.chan.amount", `1`),
		change("t.sell.provider", `"chan"`)); err != nil {
		t.Fatal(err)
	}
	err := f.apply(change("t.chan.api_key", `""`), change("t.chan.product_id", `""`), change("t.chan.amount", `0`))
	var se *settings.SectionError
	if !errors.As(err, &se) || se.Section != "t.sell" {
		t.Fatalf("clearing the selling channel should be refused by the selector's rule; got %v", err)
	}
	// Check reports the same thing without writing.
	if err := st.Check(context.Background(), []settings.Change{change("t.chan.api_key", `""`)}); err == nil {
		t.Fatal("Check should refuse what Apply refuses")
	}
	// Stored state is consistent, so CheckSections is clean.
	problems, err := st.CheckSections(context.Background())
	if err != nil || len(problems) != 0 {
		t.Fatalf("CheckSections = %v, %v", problems, err)
	}
}

// Two first writers of the same section serialize: each sees the other's
// committed values, so the rule never passes on a merged view that no longer
// exists.
func TestConcurrentFirstWritersSerialize(t *testing.T) {
	f := newFixture(t, false)
	st := f.st
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() {
			errs[i] = f.apply(
				change("t.chan.api_key", `"abcdefgh"`), change("t.chan.product_id", `"p"`), change("t.chan.amount", `1`))
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	problems, err := st.CheckSections(context.Background())
	if err != nil || len(problems) != 0 {
		t.Fatalf("after concurrent writes: %v %v", problems, err)
	}
}

// The resolver rebuilds when the section changes and not otherwise.
func TestResolverRebuildsOnlyWhenValuesChange(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, false)
	st := f.st
	builds := 0
	r := settings.NewResolver(st, func(v settings.Values) (string, error) {
		builds++
		if !v.Set("t.chan.api_key") {
			return "", errors.New("not configured")
		}
		return v.String("t.chan.product_id") + "/" + v.String("t.chan.currency"), nil
	}, "t.chan")

	if _, err := r.Get(ctx); err == nil {
		t.Fatal("unconfigured should fail to build")
	}
	if _, err := r.Get(ctx); err == nil || builds != 1 {
		t.Fatalf("a failed build must not be retried on every call: builds=%d", builds)
	}
	if err := f.apply(change("t.chan.api_key", `"abcdefgh"`), change("t.chan.product_id", `"p"`), change("t.chan.amount", `1`)); err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(ctx)
	if err != nil || got != "p/USD" || builds != 2 {
		t.Fatalf("after configuring: %q %v builds=%d", got, err, builds)
	}
	if _, _ = r.Get(ctx); builds != 2 {
		t.Fatalf("unchanged values rebuilt: builds=%d", builds)
	}
	if err := f.apply(change("t.chan.currency", `"CNY"`)); err != nil {
		t.Fatal(err)
	}
	got, _ = r.Get(ctx)
	if got != "p/CNY" || builds != 3 {
		t.Fatalf("after a change: %q builds=%d", got, builds)
	}
}

func TestMustAddRulesRejectsUnknownSections(t *testing.T) {
	for _, rule := range []settings.SectionRule{
		{Section: "t.nope", Check: func(settings.Values) error { return nil }},
		{Section: "t.chan", Reads: []string{"t.nope"}, Check: func(settings.Values) error { return nil }},
		{Section: "t.chan"},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("rule %+v should not register", rule)
				}
			}()
			settings.NewRegistry(channelSpecs()).MustAddRules(rule)
		}()
	}
}

// Readers racing a write must not leave the resolver holding the old client
// under a fingerprint it believes current. The read and the compare-and-store
// happen under one lock; without that, a reader that read old values last
// could store them after a reader that read new values, and the stale client
// would serve until the next change.
func TestResolverServesLatestAcrossConcurrentReads(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, false)
	r := settings.NewResolver(f.st, func(v settings.Values) (string, error) {
		return v.String("t.chan.product_id"), nil
	}, "t.chan")
	if err := f.apply(change("t.chan.api_key", `"abcdefgh"`), change("t.chan.product_id", `"p1"`), change("t.chan.amount", `1`)); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = r.Get(ctx)
				}
			}
		})
	}
	for i := range 20 {
		if err := f.apply(change("t.chan.product_id", fmt.Sprintf(`"p%d"`, i+2))); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if got, err := r.Get(ctx); err != nil || got != "p21" {
		t.Fatalf("after the writes the resolver must serve the latest value: %q %v", got, err)
	}
}
