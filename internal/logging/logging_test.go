package logging

import "testing"

func TestParseLevel(t *testing.T) {
	cases := map[string]string{
		"debug": "DEBUG",
		"DEBUG": "DEBUG",
		"warn":  "WARN",
		"error": "ERROR",
		"info":  "INFO",
		"":      "INFO",
		"bogus": "INFO",
	}
	for input, want := range cases {
		if got := parseLevel(input).String(); got != want {
			t.Errorf("parseLevel(%q) = %s, want %s", input, got, want)
		}
	}
}

func TestNew_ReturnsUsableLogger(t *testing.T) {
	for _, format := range []string{"text", "json", ""} {
		logger := New("debug", format)
		if logger == nil {
			t.Fatalf("New(%q) returned nil", format)
		}
		logger.Info("smoke test", "format", format)
	}
}
