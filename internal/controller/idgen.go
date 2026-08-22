package controller

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

// randomSuffix produces a short, lowercase, URL-safe random string like the
// design doc's examples (e.g. "k3n9q2").
func randomSuffix() string {
	b := make([]byte, 5) // 5 bytes -> 8 base32 chars, trimmed to 6
	_, _ = rand.Read(b)
	s := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	if len(s) > 6 {
		s = s[:6]
	}
	return s
}

func newWorkloadID() string {
	return "wl_" + randomSuffix()
}

// newInstanceID follows §4.1/§4.2's format: {workload_id}:{workload_name}:{random_suffix}
func newInstanceID(workloadID, workloadName string) string {
	return workloadID + ":" + workloadName + ":" + randomSuffix()
}
