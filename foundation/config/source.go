package config

import (
	"fmt"
	"os"
	"strings"
)

// Source is the only way configuration loaders read process configuration.
// Lookup preserves the distinction between an absent variable and a present
// empty value; ReadFile supports the conventional NAME_FILE secret source.
type Source interface {
	Lookup(name string) (string, bool)
	ReadFile(path string) ([]byte, error)
}

type osSource struct{}

func (osSource) Lookup(name string) (string, bool)    { return os.LookupEnv(name) }
func (osSource) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// OSSource returns the production configuration source.
func OSSource() Source { return osSource{} }

// MapSource is a deterministic Source for tests and config-surface recording.
// Files is keyed by the path stored in a NAME_FILE variable.
type MapSource struct {
	Values map[string]string
	Files  map[string][]byte
	Seen   map[string]bool
}

func (s MapSource) Lookup(name string) (string, bool) {
	if s.Seen != nil {
		s.Seen[name] = true
	}
	v, ok := s.Values[name]
	return v, ok
}

func (s MapSource) ReadFile(path string) ([]byte, error) {
	b, ok := s.Files[path]
	if !ok {
		return nil, fmt.Errorf("read %s: file does not exist", path)
	}
	return append([]byte(nil), b...), nil
}

// String returns a non-empty variable or def. An explicitly empty value uses
// the default too: the shipped Compose file intentionally passes blank values
// to ask the application for its built-in default.
func String(src Source, name, def string) string {
	if v, _ := src.Lookup(name); v != "" {
		return v
	}
	return def
}

// Secret resolves NAME or NAME_FILE. A non-empty value in both locations is
// rejected instead of depending on a precedence rule that can conceal stale
// credentials. Exactly one terminal line ending written by secret managers is
// removed from file-backed values.
func Secret(src Source, name string) (string, error) {
	direct, _ := src.Lookup(name)
	fileName := name + "_FILE"
	path, _ := src.Lookup(fileName)
	if direct != "" && path != "" {
		return "", fmt.Errorf("config: %s and %s must not both be set", name, fileName)
	}
	if path == "" {
		return direct, nil
	}
	b, err := src.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("config: %s: %w", fileName, err)
	}
	v := string(b)
	v = strings.TrimSuffix(v, "\n")
	v = strings.TrimSuffix(v, "\r")
	if v == "" {
		return "", fmt.Errorf("config: %s points to an empty file", fileName)
	}
	return v, nil
}
