package apiservice

import (
	"fmt"

	"github.com/golang-jwt/jwt/v5"
)

// RoutingClaims must stay field-compatible with controller.RoutingClaims —
// the two services never share Go types, but they do share this wire
// format (HMAC-SHA256, same claim names) plus the signing secret, per
// §4.2's routing token contract.
type RoutingClaims struct {
	InstanceID string `json:"instance_id"`
	GuestIP    string `json:"guest_ip"`
	GuestPort  int    `json:"guest_port"`
	HostID     string `json:"host_id"`
	jwt.RegisteredClaims
}

// TokenVerifier is the REST API Service's half of the routing token
// contract — verification only, no issuance (only the Controller issues
// tokens, on CreateInstance/ResumeInstance). This is what lets the API
// Service route directly to a guest with zero Controller round trip on the
// common warm-invoke path (§4.1/§4.2).
type TokenVerifier struct {
	secret []byte
}

func NewTokenVerifier(secret []byte) *TokenVerifier {
	return &TokenVerifier{secret: secret}
}

// Verify checks signature and expiry locally — no network call — and
// returns the claims. Any failure (bad signature, expired, malformed)
// means the caller should fall back to ResumeInstance to get a fresh token
// (§4.1's "missing/expired/verification-failure" case), whether or not the
// instance is actually suspended.
func (v *TokenVerifier) Verify(tokenStr string) (*RoutingClaims, error) {
	if tokenStr == "" {
		return nil, fmt.Errorf("empty routing token")
	}
	claims := &RoutingClaims{}
	parsed, err := jwt.ParseWithClaims(tokenStr, claims, func(tok *jwt.Token) (any, error) {
		if _, ok := tok.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", tok.Header["alg"])
		}
		return v.secret, nil
	})
	if err != nil {
		return nil, err
	}
	if !parsed.Valid {
		return nil, fmt.Errorf("invalid token")
	}
	return claims, nil
}
