package config

// Secret wraps a sensitive configuration value — an API key or signing
// secret — so it can be carried through structs and function signatures
// without silently leaking into logs or error messages. Its String and
// GoString methods always redact: fmt.Sprintf("%v", cfg) or "%+v" on a
// struct containing a Secret field prints "[REDACTED]", not the value.
//
// Call Value() only at the actual point of use (e.g. constructing an HMAC
// signer or comparing a bearer token) — never for logging or error text.
type Secret string

func (s Secret) String() string   { return "[REDACTED]" }
func (s Secret) GoString() string { return "config.Secret([REDACTED])" }

// Value returns the underlying secret.
func (s Secret) Value() string { return string(s) }

// RequiredSecret reads a required environment variable and returns it as a
// Secret. There is no fallback form — a secret with a hardcoded default
// defeats the point of it being a secret.
//
// This is the one seam in the codebase where "secret" env vars are read;
// a future move to a real secret store (GCP Secret Manager, a mounted
// secret file, Vault) only needs to change this function.
func RequiredSecret(key string) (Secret, error) {
	v, err := Required(key)
	if err != nil {
		return "", err
	}
	return Secret(v), nil
}
