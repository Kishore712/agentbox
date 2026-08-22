package controller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"agentbox/internal/common"
)

var (
	ErrWorkloadNotReady = errors.New("workload not ready")
	ErrNoHealthyHost    = errors.New("no healthy host available")
	// ErrSnapshotMissing is returned by a HostAgentClient.ResumeVM
	// implementation when the Host Agent reports the suspended instance's
	// snapshot is missing or corrupt — genuinely unrecoverable, distinct
	// from a transient network failure (§4.2 Flow — ResumeInstance).
	ErrSnapshotMissing = errors.New("snapshot missing or corrupt")
)

// ImageBuilder converts a Docker image reference into a Firecracker golden
// rootfs (§4.6). A separate interface so CreateWorkload can be unit tested
// without the real Docker/ext4 pipeline (that's Phase 2 of the
// implementation plan, built independently).
type ImageBuilder interface {
	Build(ctx context.Context, workloadID, imageRef string) (rootfsRef string, err error)
}

// buildHook, if set, fires right after CreateWorkload kicks off the async
// image build — purely a test seam so tests can synchronize without
// sleeping. Nil in production.
type buildHook func()

type Service struct {
	store  *Store
	ha     HostAgentClient
	tokens *TokenIssuer
	ib     ImageBuilder

	// round-robin placement cursor (§4.2: "v1: round-robin over a static,
	// config-driven list"). A single Controller process owns this in
	// memory — fine for the single-Controller prototype scope.
	placementCursor uint64

	onBuildStarted buildHook // test hook only
}

func NewService(store *Store, ha HostAgentClient, tokens *TokenIssuer, ib ImageBuilder) *Service {
	return &Service{store: store, ha: ha, tokens: tokens, ib: ib}
}

// --- Workload ---

type CreateWorkloadRequest struct {
	Name                   string
	ImageRef               string
	IdleTimeoutSeconds     int
	EgressAllowlist        []string
	VCPUs                  int
	MemoryMiB              int
	MaxConcurrentInstances int
}

// CreateWorkload implements §4.2 Flow — CreateWorkload: writes the record
// synchronously, kicks off the image build asynchronously, and returns
// immediately without waiting on the build.
func (svc *Service) CreateWorkload(ctx context.Context, req CreateWorkloadRequest) (*common.Workload, error) {
	w := &common.Workload{
		WorkloadID:             newWorkloadID(),
		Name:                   req.Name,
		ImageRef:               req.ImageRef,
		Status:                 common.WorkloadProvisioning,
		IdleTimeoutSeconds:     req.IdleTimeoutSeconds,
		EgressAllowlist:        req.EgressAllowlist,
		VCPUs:                  req.VCPUs,
		MemoryMiB:              req.MemoryMiB,
		MaxConcurrentInstances: req.MaxConcurrentInstances,
		CreatedAt:              time.Now().Unix(),
	}
	if err := svc.store.CreateWorkload(ctx, w); err != nil {
		return nil, fmt.Errorf("write workload record: %w", err)
	}

	go svc.runImageBuild(w.WorkloadID, w.ImageRef)
	if svc.onBuildStarted != nil {
		svc.onBuildStarted()
	}

	return w, nil
}

func (svc *Service) runImageBuild(workloadID, imageRef string) {
	ctx := context.Background()
	rootfsRef, err := svc.ib.Build(ctx, workloadID, imageRef)
	if err != nil {
		log.Printf("workload %s: image build failed: %v", workloadID, err)
		if serr := svc.store.SetWorkloadBuildResult(ctx, workloadID, common.WorkloadFailed, ""); serr != nil {
			log.Printf("workload %s: failed to record build failure: %v", workloadID, serr)
		}
		return
	}
	if err := svc.store.SetWorkloadBuildResult(ctx, workloadID, common.WorkloadReady, rootfsRef); err != nil {
		log.Printf("workload %s: failed to record build success: %v", workloadID, err)
	}
}

func (svc *Service) GetWorkload(ctx context.Context, workloadID string) (*common.Workload, error) {
	return svc.store.GetWorkload(ctx, workloadID)
}

// DeleteWorkload implements §4.2 Flow — DeleteWorkload: fire-and-forget
// cascade. Best-effort — logs failures on individual instances rather than
// aborting the whole cascade.
func (svc *Service) DeleteWorkload(ctx context.Context, workloadID string) error {
	go svc.runDeleteWorkload(context.Background(), workloadID)
	return nil
}

func (svc *Service) runDeleteWorkload(ctx context.Context, workloadID string) {
	ids, err := svc.store.WorkloadInstanceIDs(ctx, workloadID)
	if err != nil {
		log.Printf("workload %s: delete cascade: list instances: %v", workloadID, err)
		return
	}
	for _, id := range ids {
		if err := svc.runDeleteInstance(ctx, id); err != nil {
			log.Printf("workload %s: delete cascade: instance %s: %v", workloadID, id, err)
		}
	}
	if err := svc.store.DeleteWorkload(ctx, workloadID); err != nil {
		log.Printf("workload %s: delete cascade: delete workload record: %v", workloadID, err)
	}
}

// --- Instance ---

// InstanceResult mirrors the JSON bodies in §4.2's CreateInstance/
// ResumeInstance responses.
type InstanceResult struct {
	InstanceID   string
	State        common.InstanceState
	HostID       string
	GuestIP      string
	GuestPort    int
	RoutingToken string
	TokenExp     int64
	Error        string
}

// CreateInstance implements §4.2 Flow — CreateInstance. Synchronous:
// returns once the instance is RUNNING or FAILED.
func (svc *Service) CreateInstance(ctx context.Context, workloadID string) (*InstanceResult, error) {
	w, err := svc.store.GetWorkload(ctx, workloadID)
	if err != nil {
		return nil, err // ErrNotFound propagates as-is
	}
	if w.Status != common.WorkloadReady {
		return nil, ErrWorkloadNotReady
	}

	instanceID := newInstanceID(workloadID, w.Name)
	if err := svc.store.ReserveInstanceSlot(ctx, workloadID, instanceID, w.MaxConcurrentInstances); err != nil {
		return nil, err // ErrAtCapacity propagates as-is
	}

	now := time.Now().Unix()
	if err := svc.store.PutInstance(ctx, &common.Instance{
		InstanceID: instanceID,
		WorkloadID: workloadID,
		State:      common.InstanceCreating,
		CreatedAt:  now,
	}); err != nil {
		return nil, fmt.Errorf("write instance record: %w", err)
	}

	host, err := svc.pickHealthyHost(ctx)
	if err != nil {
		return svc.failInstance(ctx, instanceID, common.InstanceCreating, err)
	}

	ep, err := svc.ha.BootVM(ctx, host.InternalAddr, BootVMRequest{
		InstanceID:      instanceID,
		RootfsRef:       w.RootfsRef,
		VCPUs:           w.VCPUs,
		MemoryMiB:       w.MemoryMiB,
		EgressAllowlist: w.EgressAllowlist,
	})
	if err != nil {
		return svc.failInstance(ctx, instanceID, common.InstanceCreating, err)
	}

	return svc.finishBootOrResume(ctx, instanceID, common.InstanceCreating, host.HostID, ep, w.IdleTimeoutSeconds)
}

func (svc *Service) GetInstance(ctx context.Context, instanceID string) (*common.Instance, error) {
	return svc.store.GetInstance(ctx, instanceID)
}

// SuspendInstance implements §4.2 Flow — SuspendInstance. Called only by
// the Controller's own internal idle-reaper loop (§4.2), never by the API
// Service. No-op (not an error) if the instance isn't RUNNING.
func (svc *Service) SuspendInstance(ctx context.Context, instanceID string) error {
	swapped, _, err := svc.store.CAS(ctx, instanceID, common.InstanceRunning, common.InstanceSuspending)
	if err != nil {
		return err
	}
	if !swapped {
		return nil // already transitioning or not RUNNING; nothing to do
	}

	inst, err := svc.store.GetInstance(ctx, instanceID)
	if err != nil {
		return err
	}
	host, err := svc.store.GetHost(ctx, inst.HostID)
	if err != nil {
		return err
	}

	if err := svc.ha.SuspendVM(ctx, host.InternalAddr, instanceID); err != nil {
		// Revert to RUNNING rather than FAILED — the instance is still
		// alive, just not yet reclaimed. The idle-reaper loop retries next tick.
		if _, _, cerr := svc.store.CAS(ctx, instanceID, common.InstanceSuspending, common.InstanceRunning); cerr != nil {
			log.Printf("instance %s: failed to revert SUSPENDING->RUNNING after suspend failure: %v", instanceID, cerr)
		}
		return err
	}

	if _, _, err := svc.store.CAS(ctx, instanceID, common.InstanceSuspending, common.InstanceSuspended); err != nil {
		return err
	}
	if err := svc.store.ClearDue(ctx, instanceID); err != nil {
		log.Printf("instance %s: failed to clear instances_due after suspend: %v", instanceID, err)
	}
	if err := svc.store.AdjustHostCapacity(ctx, inst.HostID, -1); err != nil {
		log.Printf("instance %s: failed to decrement host capacity after suspend: %v", instanceID, err)
	}
	return nil
}

// ResumeInstance implements §4.2 Flow — ResumeInstance. Synchronous and
// idempotent: safe to call even if the instance turns out to still be
// RUNNING (just returns fresh routing info instead of performing a real
// resume) — this is what lets it double as the REST API Service's
// token-refresh fallback (§4.1/§4.2 "Routing token contract"), not only the
// genuinely-suspended case.
func (svc *Service) ResumeInstance(ctx context.Context, instanceID string) (*InstanceResult, error) {
	swapped, actual, err := svc.store.CAS(ctx, instanceID, common.InstanceSuspended, common.InstanceResuming)
	if err != nil {
		return nil, err
	}

	if !swapped {
		switch actual {
		case "":
			return nil, ErrNotFound
		case common.InstanceRunning:
			return svc.currentResultWithFreshToken(ctx, instanceID)
		case common.InstanceResuming:
			return svc.waitForResumeToSettle(ctx, instanceID)
		default:
			// FAILED, DELETING, CREATING, SUSPENDING: nothing to resume into.
			inst, gerr := svc.store.GetInstance(ctx, instanceID)
			if gerr != nil {
				return nil, gerr
			}
			return &InstanceResult{InstanceID: instanceID, State: inst.State, Error: inst.Error}, nil
		}
	}

	// We own the SUSPENDED->RESUMING transition; resume must happen on the
	// same host the snapshot lives on (§4.7/§4.2) — no re-placement.
	inst, err := svc.store.GetInstance(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	w, err := svc.store.GetWorkload(ctx, inst.WorkloadID)
	if err != nil {
		return nil, err
	}
	host, err := svc.store.GetHost(ctx, inst.HostID)
	if err != nil {
		return nil, err
	}

	ep, err := svc.ha.ResumeVM(ctx, host.InternalAddr, instanceID)
	if err != nil {
		if errors.Is(err, ErrSnapshotMissing) {
			return svc.failInstance(ctx, instanceID, common.InstanceResuming, err)
		}
		// Transient/unreachable: revert to SUSPENDED, not FAILED — the
		// snapshot on disk is untouched, a later invoke just retries.
		if _, _, cerr := svc.store.CAS(ctx, instanceID, common.InstanceResuming, common.InstanceSuspended); cerr != nil {
			log.Printf("instance %s: failed to revert RESUMING->SUSPENDED after resume failure: %v", instanceID, cerr)
		}
		return nil, err
	}

	return svc.finishBootOrResume(ctx, instanceID, common.InstanceResuming, inst.HostID, ep, w.IdleTimeoutSeconds)
}

func (svc *Service) currentResultWithFreshToken(ctx context.Context, instanceID string) (*InstanceResult, error) {
	inst, err := svc.store.GetInstance(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	w, err := svc.store.GetWorkload(ctx, inst.WorkloadID)
	if err != nil {
		return nil, err
	}
	tok, exp, err := svc.tokens.Issue(instanceID, inst.GuestIP, inst.GuestPort, inst.HostID, w.IdleTimeoutSeconds)
	if err != nil {
		return nil, err
	}
	return &InstanceResult{
		InstanceID: instanceID, State: inst.State, HostID: inst.HostID,
		GuestIP: inst.GuestIP, GuestPort: inst.GuestPort,
		RoutingToken: tok, TokenExp: exp,
	}, nil
}

// waitForResumeToSettle implements the CAS-flow note: "if already RESUMING
// (a concurrent resume in flight), wait briefly for that transition to
// settle rather than starting a second one."
func (svc *Service) waitForResumeToSettle(ctx context.Context, instanceID string) (*InstanceResult, error) {
	const pollInterval = 25 * time.Millisecond
	const timeout = 5 * time.Second
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		inst, err := svc.store.GetInstance(ctx, instanceID)
		if err != nil {
			return nil, err
		}
		if inst.State != common.InstanceResuming {
			return svc.currentResultWithFreshToken(ctx, instanceID)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
	return nil, fmt.Errorf("instance %s: timed out waiting for concurrent resume to settle", instanceID)
}

// finishBootOrResume is the shared tail of CreateInstance and
// ResumeInstance's success path: CAS into RUNNING, persist the endpoint,
// touch activity, account host capacity, issue a routing token.
func (svc *Service) finishBootOrResume(ctx context.Context, instanceID string, from common.InstanceState, hostID string, ep VMEndpoint, idleTimeoutSeconds int) (*InstanceResult, error) {
	if _, _, err := svc.store.CAS(ctx, instanceID, from, common.InstanceRunning); err != nil {
		return nil, err
	}
	if err := svc.store.UpdateInstanceFields(ctx, instanceID, map[string]any{
		"host_id":    hostID,
		"guest_ip":   ep.GuestIP,
		"guest_port": ep.GuestPort,
	}); err != nil {
		return nil, err
	}
	if err := svc.store.TouchActivity(ctx, instanceID, idleTimeoutSeconds); err != nil {
		log.Printf("instance %s: failed to touch activity after boot/resume: %v", instanceID, err)
	}
	if err := svc.store.AdjustHostCapacity(ctx, hostID, 1); err != nil {
		log.Printf("instance %s: failed to increment host capacity: %v", instanceID, err)
	}
	tok, exp, err := svc.tokens.Issue(instanceID, ep.GuestIP, ep.GuestPort, hostID, idleTimeoutSeconds)
	if err != nil {
		return nil, err
	}
	return &InstanceResult{
		InstanceID: instanceID, State: common.InstanceRunning, HostID: hostID,
		GuestIP: ep.GuestIP, GuestPort: ep.GuestPort,
		RoutingToken: tok, TokenExp: exp,
	}, nil
}

// failInstance CASes into FAILED and records the error — used when boot or
// a snapshot-missing resume can't proceed. Per §4.2: no retry, leave the
// record for the client to inspect/delete.
func (svc *Service) failInstance(ctx context.Context, instanceID string, from common.InstanceState, cause error) (*InstanceResult, error) {
	if _, _, err := svc.store.CAS(ctx, instanceID, from, common.InstanceFailed); err != nil {
		return nil, err
	}
	if err := svc.store.UpdateInstanceFields(ctx, instanceID, map[string]any{"error": cause.Error()}); err != nil {
		log.Printf("instance %s: failed to record failure reason: %v", instanceID, err)
	}
	return &InstanceResult{InstanceID: instanceID, State: common.InstanceFailed, Error: cause.Error()}, nil
}

// DeleteInstance implements §4.2 Flow — DeleteInstance: fire-and-forget.
func (svc *Service) DeleteInstance(ctx context.Context, instanceID string) error {
	go func() {
		if err := svc.runDeleteInstance(context.Background(), instanceID); err != nil {
			log.Printf("instance %s: delete failed: %v", instanceID, err)
		}
	}()
	return nil
}

func (svc *Service) runDeleteInstance(ctx context.Context, instanceID string) error {
	inst, err := svc.store.GetInstance(ctx, instanceID)
	if err != nil {
		return err
	}
	if err := svc.store.SetInstanceState(ctx, instanceID, common.InstanceDeleting); err != nil {
		return err
	}

	var host *common.Host
	if inst.HostID != "" {
		host, err = svc.store.GetHost(ctx, inst.HostID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}

	if host != nil {
		const maxAttempts = 3
		var lastErr error
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			if lastErr = svc.ha.DeleteVM(ctx, host.InternalAddr, instanceID); lastErr == nil {
				break
			}
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
		}
		if lastErr != nil {
			// Persistent failure: leave the record in DELETING for manual
			// cleanup (§4.2) — orphan reconciliation isn't built in v1.
			return fmt.Errorf("host agent delete failed after %d attempts, leaving instance in DELETING: %w", maxAttempts, lastErr)
		}
	}

	if inst.State == common.InstanceRunning {
		if err := svc.store.AdjustHostCapacity(ctx, inst.HostID, -1); err != nil {
			log.Printf("instance %s: failed to decrement host capacity on delete: %v", instanceID, err)
		}
	}
	return svc.store.DeleteInstance(ctx, inst.WorkloadID, instanceID)
}

// Heartbeat implements the async, fire-and-forget POST
// /internal/instances/{id}/heartbeat (§4.2): bumps last_active and refreshes
// the idle-reaper's due-score. It's the only remaining reason the REST API
// Service talks to the Controller on a warm RUNNING request, decoupled from
// routing so it never blocks the client's response.
func (svc *Service) Heartbeat(ctx context.Context, instanceID string) error {
	inst, err := svc.store.GetInstance(ctx, instanceID)
	if err != nil {
		return err
	}
	w, err := svc.store.GetWorkload(ctx, inst.WorkloadID)
	if err != nil {
		return err
	}
	return svc.store.TouchActivity(ctx, instanceID, w.IdleTimeoutSeconds)
}

// --- Placement ---

func (svc *Service) pickHealthyHost(ctx context.Context) (*common.Host, error) {
	hosts, err := svc.store.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	var healthy []common.Host
	for _, h := range hosts {
		if h.Status == common.HostHealthy {
			healthy = append(healthy, h)
		}
	}
	if len(healthy) == 0 {
		return nil, ErrNoHealthyHost
	}
	idx := atomic.AddUint64(&svc.placementCursor, 1) - 1
	return &healthy[idx%uint64(len(healthy))], nil
}
