package snapshot

import (
	"fmt"
	"os"
	"strings"
)

// Secrets resolves a backend's credential reference to its value.
type Secrets interface {
	// Secret returns the value of a "name/key" reference or of an
	// environment variable (standalone).
	Secret(ref, env string) (string, error)
}

// EnvSecrets resolves standalone credentials from the environment, and
// refuses Secret references (there is no cluster).
type EnvSecrets struct{ Getenv func(string) string }

// Secret returns the environment variable env, trimmed.
func (e EnvSecrets) Secret(ref, env string) (string, error) {
	if env == "" {
		return "", fmt.Errorf("secretRef %s: standalone mode reads credentials from environment variables (spec.auth.env)", ref)
	}
	v := e.Getenv(env)
	if v == "" {
		return "", fmt.Errorf("environment variable %s is not set", env)
	}
	return strings.TrimSpace(v), nil
}

// FileSecrets resolves secretRef "name/key" to the file key in the
// directory a Secret name is mounted at, under Root (production: each
// Secret mounted at /var/run/golem/secrets/<name>). Env credentials are
// refused: they are standalone's only.
type FileSecrets struct{ Root *os.Root }

// Secret reads the mounted file of ref, trimmed.
func (f FileSecrets) Secret(ref, env string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("environment credential %s: only standalone mode reads the environment", env)
	}
	name, key, _ := strings.Cut(ref, "/")
	b, err := f.Root.ReadFile(name + "/" + key)
	if err != nil {
		return "", fmt.Errorf("secret %s: %w (is the Secret mounted?)", ref, err)
	}
	return strings.TrimSpace(string(b)), nil
}
