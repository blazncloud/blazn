package listener

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
)

const tokenBytes = 32

// Credential is an activation-local listener credential. Only
// GenerateCredential can create one; callers cannot assert randomness by
// supplying token-shaped text.
type Credential struct{ value string }

func (Credential) String() string   { return "[REDACTED]" }
func (Credential) GoString() string { return "[REDACTED]" }

func GenerateCredential() (Credential, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return Credential{}, err
	}
	return Credential{value: base64.RawURLEncoding.EncodeToString(raw)}, nil
}

func validateCredential(value string) error {
	if value == "" || strings.Contains(value, "=") {
		return errors.New("listener credential must be unpadded base64url")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(raw) < tokenBytes || base64.RawURLEncoding.EncodeToString(raw) != value {
		return errors.New("listener credential must contain at least 256 random bits as unpadded base64url")
	}
	return nil
}

// ChildEnvironment returns a copy with only the documented source credential
// variables replaced. The credential remains in memory and the child process
// environment; it is never represented in argv or persistent state.
func (c Credential) ChildEnvironment(base []string) ([]string, error) {
	if err := validateCredential(c.value); err != nil {
		return nil, err
	}
	names := map[string]bool{
		"OPENAI_API_KEY": true, "ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true,
	}
	result := make([]string, 0, len(base)+3)
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if !names[name] {
			result = append(result, entry)
		}
	}
	result = append(result,
		"OPENAI_API_KEY="+c.value,
		"ANTHROPIC_API_KEY="+c.value,
		"ANTHROPIC_AUTH_TOKEN="+c.value,
	)
	return result, nil
}

func (c Credential) authenticateValue() string { return c.value }
