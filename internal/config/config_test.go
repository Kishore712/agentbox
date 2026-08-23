package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestString_FallbackWhenUnset(t *testing.T) {
	if got := String("AGENTBOX_TEST_STRING_UNSET", "fallback"); got != "fallback" {
		t.Errorf("got %q, want fallback", got)
	}
}

func TestString_UsesSetValue(t *testing.T) {
	t.Setenv("AGENTBOX_TEST_STRING", "explicit")
	if got := String("AGENTBOX_TEST_STRING", "fallback"); got != "explicit" {
		t.Errorf("got %q, want explicit", got)
	}
}

func TestInt_FallbackWhenUnset(t *testing.T) {
	got, err := Int("AGENTBOX_TEST_INT_UNSET", 42)
	if err != nil {
		t.Fatal(err)
	}
	if got != 42 {
		t.Errorf("got %d, want 42", got)
	}
}

func TestInt_MalformedValueIsAnError(t *testing.T) {
	t.Setenv("AGENTBOX_TEST_INT_BAD", "not-a-number")
	_, err := Int("AGENTBOX_TEST_INT_BAD", 42)
	if err == nil {
		t.Fatal("expected an error for a malformed integer, got nil")
	}
	if !strings.Contains(err.Error(), "AGENTBOX_TEST_INT_BAD") {
		t.Errorf("error should name the offending variable: %v", err)
	}
}

func TestDuration_ParsesSecondsWhenSet(t *testing.T) {
	t.Setenv("AGENTBOX_TEST_DURATION", "90")
	got, err := Duration("AGENTBOX_TEST_DURATION", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got != 90*time.Second {
		t.Errorf("got %s, want 90s", got)
	}
}

func TestBool_MalformedValueIsAnError(t *testing.T) {
	t.Setenv("AGENTBOX_TEST_BOOL_BAD", "yeah")
	_, err := Bool("AGENTBOX_TEST_BOOL_BAD", false)
	if err == nil {
		t.Fatal("expected an error for a malformed bool, got nil")
	}
}

func TestRequired_MissingIsAnError(t *testing.T) {
	_, err := Required("AGENTBOX_TEST_REQUIRED_UNSET")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "AGENTBOX_TEST_REQUIRED_UNSET") {
		t.Errorf("error should name the missing variable: %v", err)
	}
}

func TestRequiredSecret_ValueRoundTrips(t *testing.T) {
	t.Setenv("AGENTBOX_TEST_SECRET", "top-secret-value")
	s, err := RequiredSecret("AGENTBOX_TEST_SECRET")
	if err != nil {
		t.Fatal(err)
	}
	if s.Value() != "top-secret-value" {
		t.Errorf("Value() = %q, want top-secret-value", s.Value())
	}
}

func TestSecret_StringIsRedacted(t *testing.T) {
	s := Secret("do-not-leak-me")
	if got := s.String(); strings.Contains(got, "do-not-leak-me") {
		t.Fatalf("String() leaked the secret value: %q", got)
	}
	wrapper := struct{ S Secret }{S: s}
	formatted := fmt.Sprintf("%+v", wrapper)
	if strings.Contains(formatted, "do-not-leak-me") {
		t.Fatalf("%%+v on a struct containing a Secret leaked the value: %q", formatted)
	}
}
