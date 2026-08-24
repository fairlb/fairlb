// gate-honesty: two tests here skip on Windows, and one also skips when running
// as root — both assert POSIX permission bits, which those platforms do not
// have (or, as root, do not enforce). The suite would then report "passed" with
// those assertions never executed, so what backs the conclusion is where it
// runs: CI and every developer machine here are Linux or macOS as a non-root
// user, where both execute. Nothing else in this file is skipped, and the
// property the feature actually rests on — a key that survives a restart, and a
// corrupt key file that stops start-up instead of being replaced — is asserted
// unconditionally on every platform.
package bootstrap_test

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fairlb/fairlb/foundation/crypto"
	"github.com/fairlb/fairlb/internal/community/bootstrap"
)

// The property the whole feature rests on: whatever was encrypted before a
// restart is still readable after one. Asserting the bytes match would prove
// less — it would pass just as well if both runs returned a zero key.
func TestSecretKeySurvivesRestartAndKeepsCiphertextReadable(t *testing.T) {
	dir := t.TempDir()

	first, err := bootstrap.LoadOrCreateSecretKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	box, err := crypto.NewBox(first)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal([]byte("sk-upstream-secret"), nil)
	if err != nil {
		t.Fatal(err)
	}

	// A second process against the same volume.
	second, err := bootstrap.LoadOrCreateSecretKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	box2, err := crypto.NewBox(second)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := box2.Open(sealed, nil)
	if err != nil {
		t.Fatalf("credentials encrypted before the restart are unreadable after it: %v", err)
	}
	if string(plain) != "sk-upstream-secret" {
		t.Fatalf("decrypted %q", plain)
	}
}

// A fresh directory must not produce a predictable key.
func TestSecretKeyIsFreshPerDirectory(t *testing.T) {
	a, err := bootstrap.LoadOrCreateSecretKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := bootstrap.LoadOrCreateSecretKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 32 {
		t.Fatalf("key is %d bytes, want 32", len(a))
	}
	if string(a) == string(b) {
		t.Fatal("two independent data directories produced the same key")
	}
}

func TestSecretKeyFileIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	dir := t.TempDir()
	if _, err := bootstrap.LoadOrCreateSecretKey(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, bootstrap.SecretKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode is %o, want 600", perm)
	}
}

// The dangerous branch: a key file that cannot be parsed must stop the process,
// never be replaced. Regenerating would turn "wrong volume mounted" into
// permanently unreadable credentials, and it would do it silently.
func TestUnreadableSecretKeyIsAnErrorRatherThanReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, bootstrap.SecretKeyFile)
	if err := os.WriteFile(path, []byte("not-a-key"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := bootstrap.LoadOrCreateSecretKey(dir)
	if err == nil {
		t.Fatal("a corrupt key file was accepted")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != "not-a-key" {
		t.Fatalf("the corrupt key file was overwritten (now %q) — the original is gone", after)
	}
}

// Trailing newlines are what a key file gets when someone creates it by hand
// with `openssl rand -hex 32 > secret.key`.
func TestSecretKeyToleratesTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	want := make([]byte, 32)
	for i := range want {
		want[i] = byte(i + 1)
	}
	if err := os.WriteFile(filepath.Join(dir, bootstrap.SecretKeyFile),
		[]byte(hex.EncodeToString(want)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := bootstrap.LoadOrCreateSecretKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("a key written with a trailing newline was not read back")
	}
}

func TestUnwritableDataDirExplainsBothWaysOut(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions and a non-root user")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	_, err := bootstrap.LoadOrCreateSecretKey(filepath.Join(parent, "data"))
	if err == nil {
		t.Fatal("writing to a read-only directory succeeded")
	}
	// The bare errno tells the operator nothing actionable, so the message has
	// to name both remedies.
	for _, want := range []string{"SECRET_KEY", "volume"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q; it reads: %v", want, err)
		}
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("the underlying cause was not wrapped: %v", err)
	}
}
