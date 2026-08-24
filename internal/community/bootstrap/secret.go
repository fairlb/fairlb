package bootstrap

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/fairlb/fairlb/foundation/crypto"
)

// SecretKeyFile is where a generated master key is kept, relative to the data
// directory.
const SecretKeyFile = "secret.key"

// LoadOrCreateSecretKey returns the master key used to encrypt provider
// credentials at rest, generating and storing one on first start.
//
// # Why this exists
//
// The alternative that documentation tends to reach for is
// `SECRET_KEY="$(openssl rand -hex 32)"` in the run command. That works exactly
// once. The next `docker run` mints a different key, and every provider
// credential already in the database becomes undecryptable — with no error at
// start-up, because nothing tries to decrypt anything until a request needs an
// upstream key. What the operator sees is a gateway that authenticated fine
// yesterday and cannot reach any provider today.
//
// So an unset SECRET_KEY means "keep one for me": generate it once, store it in
// the data directory, reuse it forever. This is what Gitea, n8n and Vaultwarden
// all do, and for the same reason.
//
// # Rules
//
//   - An explicit SECRET_KEY always wins; this function is not called then.
//     Multi-replica deployments must use it, since replicas do not share a
//     data directory.
//   - A key file that exists but cannot be read or parsed is a hard error. The
//     tempting alternative — generate a fresh one and carry on — turns a
//     recoverable mistake (wrong volume mounted) into permanent data loss.
//   - The file is written 0600. It is the key to every credential in the
//     database.
func LoadOrCreateSecretKey(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, SecretKeyFile)

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		key, decErr := crypto.DecodeKeyHex(trimSpace(string(raw)))
		if decErr != nil {
			return nil, fmt.Errorf(
				"bootstrap: %s exists but is not a valid key (%w).\n"+
					"  Not regenerating: every provider credential in the database is\n"+
					"  encrypted with the original key, and replacing it would make them\n"+
					"  permanently unreadable. Restore the file, or point FAIRLB_DATA_DIR\n"+
					"  at the directory that has it, or set SECRET_KEY explicitly",
				path, decErr)
		}
		return key, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, fmt.Errorf("bootstrap: reading %s: %w", path, err)
	}

	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, dataDirError(dataDir, err)
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("bootstrap: generating a master key: %w", err)
	}
	// O_EXCL: if two processes start against the same volume at once, exactly
	// one creates the key and the other reports it rather than overwriting a
	// key the first one may already have encrypted something with.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf(
				"bootstrap: %s appeared while starting up — another instance is "+
					"using this data directory; start one at a time", path)
		}
		return nil, dataDirError(dataDir, err)
	}
	_, writeErr := f.WriteString(hex.EncodeToString(buf))
	closeErr := f.Close()
	if writeErr != nil {
		return nil, fmt.Errorf("bootstrap: writing %s: %w", path, writeErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("bootstrap: writing %s: %w", path, closeErr)
	}
	return buf, nil
}

// dataDirError explains the two ways out, because the underlying error is
// usually a bare "permission denied" against a path the operator never typed.
func dataDirError(dataDir string, err error) error {
	return fmt.Errorf(
		"bootstrap: cannot write to the data directory %s (%w).\n"+
			"  This is where the generated master key is kept, so it has to survive\n"+
			"  restarts. Either mount a writable volume there (or point\n"+
			"  FAIRLB_DATA_DIR somewhere writable), or set SECRET_KEY yourself\n"+
			"  with: openssl rand -hex 32",
		dataDir, err)
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
