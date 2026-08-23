// Package config centralizes how every agentbox service reads its runtime
// configuration from the environment. Before this package existed, each of
// the three cmd/*/main.go files carried its own copy-pasted envOr/envIntOr/
// envDurationOr helpers, hardcoded default literals inline in main(), and
// plain os.Getenv calls for secrets indistinguishable from ordinary config.
//
// The rule this package enforces: a missing (empty) environment variable
// silently falls back to the documented default, but a present-but-malformed
// value (e.g. GUEST_PORT=abc) is a startup error, not a silently-ignored
// warning — an operator who set a value clearly intended it to take effect.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// String returns the named environment variable, or fallback if it is unset
// or empty.
func String(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Int returns the named environment variable parsed as an integer, or
// fallback if it is unset or empty. A value that is set but not a valid
// integer is a configuration error, not a fallback case.
func Int(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("environment variable %s=%q must be an integer: %w", key, v, err)
	}
	return n, nil
}

// Bool returns the named environment variable parsed as a boolean, or
// fallback if it is unset or empty. Accepts the same forms as
// strconv.ParseBool ("1", "t", "T", "TRUE", "true", "True", and their
// false equivalents).
func Bool(key string, fallback bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("environment variable %s=%q must be a boolean (true/false): %w", key, v, err)
	}
	return b, nil
}

// Duration returns the named environment variable, interpreted as a whole
// number of seconds, or fallback if it is unset or empty.
func Duration(key string, fallback time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	secs, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("environment variable %s=%q must be an integer number of seconds: %w", key, v, err)
	}
	return time.Duration(secs) * time.Second, nil
}

// Required returns the named environment variable, or an error naming
// exactly which variable is missing. Use this for values that have no safe
// default — the caller must supply them.
func Required(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("required environment variable %s is not set", key)
	}
	return v, nil
}
