package controller

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TokenIssuer signs and verifies the routing tokens described in §4.2's
// "Routing token contract" — HMAC-SHA256, claims {instance_id, guest_ip,
// guest_port, host_id, exp}, shared secret between Controller and the REST
// API Service. This is what lets the API Service route directly to a guest
// without a Controller round trip on every invoke.
type TokenIssuer struct {
	secret []byte
}

func NewTokenIssuer(secret []byte) *TokenIssuer {
	return &TokenIssuer{secret: secret}
}

type RoutingClaims struct {
	InstanceID string `json:"instance_id"`
	GuestIP    string `json:"guest_ip"`
	GuestPort  int    `json:"guest_port"`
	HostID     string `json:"host_id"`
	jwt.RegisteredClaims
}

// TTL implements §4.2's rule: min(idle_timeout_seconds/2, 60s) — short
// enough that a token can't badly outlive a heartbeat, long enough to avoid
// refreshing constantly on a busy instance.
func TTL(idleTimeoutSeconds int) time.Duration {
	half := time.Duration(idleTimeoutSeconds) * time.Second / 2
	max := 60 * time.Second
	if half < max {
		return half
	}
	return max
}

// Issue signs a routing token for the given instance/endpoint, valid for TTL(idleTimeoutSeconds).
func (t *TokenIssuer) Issue(instanceID, guestIP string, guestPort int, hostID string, idleTimeoutSeconds int) (token string, exp int64, err error) {
	expiry := time.Now().Add(TTL(idleTimeoutSeconds))
	claims := RoutingClaims{
		InstanceID: instanceID,
		GuestIP:    guestIP,
		GuestPort:  guestPort,
		HostID:     hostID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiry),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(t.secret)
	if err != nil {
		return "", 0, fmt.Errorf("sign routing token: %w", err)
	}
	return signed, expiry.Unix(), nil
}

// Verify checks signature and expiry and returns the claims. This is the
// call the REST API Service makes locally, with no network round trip, on
// every warm invoke.
func (t *TokenIssuer) Verify(tokenStr string) (*RoutingClaims, error) {
	claims := &RoutingClaims{}
	parsed, err := jwt.ParseWithClaims(tokenStr, claims, func(tok *jwt.Token) (any, error) {
		if _, ok := tok.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", tok.Header["alg"])
		}
		return t.secret, nil
	})
	if err != nil {
		return nil, err
	}
	if !parsed.Valid {
		return nil, fmt.Errorf("invalid token")
	}
	return claims, nil
}
