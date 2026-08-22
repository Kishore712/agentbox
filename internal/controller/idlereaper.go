package controller

import (
	"context"
	"log"
	"time"
)

// IdleReaper is the Controller's internal background loop (§4.2
// "Idle-reaper loop") — not a separate component, not called by anyone
// externally. It exists only because it needs to be started somewhere; the
// actual suspend logic is Service.SuspendInstance, called here as a plain
// in-process function call, no network hop.
type IdleReaper struct {
	svc      *Service
	interval time.Duration
}

func NewIdleReaper(svc *Service, interval time.Duration) *IdleReaper {
	return &IdleReaper{svc: svc, interval: interval}
}

// Run blocks, ticking every interval until ctx is canceled.
func (r *IdleReaper) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Tick(ctx)
		}
	}
}

// Tick runs one scan-and-suspend pass: ZRANGEBYSCORE instances_due -inf now,
// then Service.SuspendInstance on each match (§4.2). Exported separately
// from Run so tests can drive individual ticks deterministically instead of
// waiting on a real timer.
func (r *IdleReaper) Tick(ctx context.Context) {
	due, err := r.svc.store.DueInstances(ctx)
	if err != nil {
		log.Printf("idle reaper: DueInstances: %v", err)
		return
	}
	for _, instanceID := range due {
		if err := r.svc.SuspendInstance(ctx, instanceID); err != nil {
			log.Printf("idle reaper: SuspendInstance(%s): %v", instanceID, err)
		}
	}
}
