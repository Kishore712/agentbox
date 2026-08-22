package hostagent

import (
	"context"
	"fmt"
	"net"
	"time"
)

// ReadinessChecker implements §4.3 step 6: poll a TCP connect until the
// guest's app is actually accepting connections, no guest cooperation
// needed.
type ReadinessChecker interface {
	WaitReady(ctx context.Context, addr string, timeout time.Duration) error
}

type TCPReadinessChecker struct {
	PollInterval time.Duration
}

func (c *TCPReadinessChecker) WaitReady(ctx context.Context, addr string, timeout time.Duration) error {
	interval := c.PollInterval
	if interval == 0 {
		interval = 50 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		d := net.Dialer{Timeout: interval}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return fmt.Errorf("timed out waiting for %s to accept connections: %w", addr, lastErr)
}
