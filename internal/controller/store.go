// Package controller implements the Controller service: the generic
// compute-provisioning layer described in Docs/agent-sandbox-platform-design-v3.md §4.2.
// It owns Workload registration and the Instance lifecycle state machine, and
// is the sole owner of the Redis data store — no other service ever connects
// to it directly (§4.4).
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"agentbox/internal/common"
)

var (
	// ErrNotFound is returned when a workload/instance/host record doesn't exist.
	ErrNotFound = errors.New("not found")
	// ErrAtCapacity is returned when a workload is already at max_concurrent_instances.
	ErrAtCapacity = errors.New("at capacity")
)

// Store is the Controller's Redis-backed data layer. It implements the
// schema and atomicity approach from §4.2 (Lua-scripted compare-and-swap for
// every state transition, so concurrent Resume/Suspend/Create calls can't
// race each other).
type Store struct {
	rdb *redis.Client
}

func NewStore(rdb *redis.Client) *Store {
	return &Store{rdb: rdb}
}

func workloadKey(id string) string          { return "workload:" + id }
func instanceKey(id string) string          { return "instance:" + id }
func workloadInstancesKey(id string) string { return "workload_instances:" + id }
func hostKey(id string) string              { return "host:" + id }

const instancesDueKey = "instances_due"

// --- Workload ---

func (s *Store) CreateWorkload(ctx context.Context, w *common.Workload) error {
	allowlist, err := json.Marshal(w.EgressAllowlist)
	if err != nil {
		return fmt.Errorf("marshal egress_allowlist: %w", err)
	}
	fields := map[string]any{
		"name":                     w.Name,
		"image_ref":                w.ImageRef,
		"status":                   string(w.Status),
		"rootfs_ref":               w.RootfsRef,
		"idle_timeout_seconds":     w.IdleTimeoutSeconds,
		"egress_allowlist":         string(allowlist),
		"vcpus":                    w.VCPUs,
		"memory_mib":               w.MemoryMiB,
		"max_concurrent_instances": w.MaxConcurrentInstances,
		"created_at":               w.CreatedAt,
	}
	return s.rdb.HSet(ctx, workloadKey(w.WorkloadID), fields).Err()
}

func (s *Store) GetWorkload(ctx context.Context, workloadID string) (*common.Workload, error) {
	res, err := s.rdb.HGetAll(ctx, workloadKey(workloadID)).Result()
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, ErrNotFound
	}
	var allowlist []string
	if v := res["egress_allowlist"]; v != "" {
		if err := json.Unmarshal([]byte(v), &allowlist); err != nil {
			return nil, fmt.Errorf("unmarshal egress_allowlist: %w", err)
		}
	}
	idleTimeout, _ := strconv.Atoi(res["idle_timeout_seconds"])
	vcpus, _ := strconv.Atoi(res["vcpus"])
	memory, _ := strconv.Atoi(res["memory_mib"])
	maxConcurrent, _ := strconv.Atoi(res["max_concurrent_instances"])
	createdAt, _ := strconv.ParseInt(res["created_at"], 10, 64)
	return &common.Workload{
		WorkloadID:             workloadID,
		Name:                   res["name"],
		ImageRef:               res["image_ref"],
		Status:                 common.WorkloadStatus(res["status"]),
		RootfsRef:              res["rootfs_ref"],
		IdleTimeoutSeconds:     idleTimeout,
		EgressAllowlist:        allowlist,
		VCPUs:                  vcpus,
		MemoryMiB:              memory,
		MaxConcurrentInstances: maxConcurrent,
		CreatedAt:              createdAt,
	}, nil
}

func (s *Store) SetWorkloadStatus(ctx context.Context, workloadID string, status common.WorkloadStatus) error {
	n, err := s.rdb.Exists(ctx, workloadKey(workloadID)).Result()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return s.rdb.HSet(ctx, workloadKey(workloadID), "status", string(status)).Err()
}

// SetWorkloadBuildResult records the Image Builder's outcome (§4.2 Flow —
// CreateWorkload step 4): READY + the golden rootfs path, or FAILED with no
// usable rootfs.
func (s *Store) SetWorkloadBuildResult(ctx context.Context, workloadID string, status common.WorkloadStatus, rootfsRef string) error {
	n, err := s.rdb.Exists(ctx, workloadKey(workloadID)).Result()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return s.rdb.HSet(ctx, workloadKey(workloadID), "status", string(status), "rootfs_ref", rootfsRef).Err()
}

// DeleteWorkload removes the workload record and its instance-id set. It
// does NOT cascade-delete instances or the rootfs file — that orchestration
// belongs to the Controller service layer (§4.2 Flow — DeleteWorkload),
// which enumerates workload_instances first and tears each down via the
// Host Agent before calling this.
func (s *Store) DeleteWorkload(ctx context.Context, workloadID string) error {
	pipe := s.rdb.Pipeline()
	pipe.Del(ctx, workloadKey(workloadID))
	pipe.Del(ctx, workloadInstancesKey(workloadID))
	_, err := pipe.Exec(ctx)
	return err
}

func (s *Store) WorkloadInstanceIDs(ctx context.Context, workloadID string) ([]string, error) {
	return s.rdb.SMembers(ctx, workloadInstancesKey(workloadID)).Result()
}

// --- Instance ---

// createInstanceScript atomically checks the concurrency cap and reserves a
// slot for the new instance id in one round trip. Redis executes Lua
// scripts atomically and single-threaded, so this fully closes the
// check-then-act race described in §4.2 — not just narrows it.
var createInstanceScript = redis.NewScript(`
local count = redis.call('SCARD', KEYS[1])
if count >= tonumber(ARGV[2]) then
  return 0
end
redis.call('SADD', KEYS[1], ARGV[1])
return 1
`)

// ReserveInstanceSlot performs the Lua cap-check-and-reserve from §4.2's
// CreateInstance flow step 2. Returns ErrAtCapacity if the workload is
// already at max_concurrent_instances.
func (s *Store) ReserveInstanceSlot(ctx context.Context, workloadID, instanceID string, maxConcurrent int) error {
	res, err := createInstanceScript.Run(ctx, s.rdb, []string{workloadInstancesKey(workloadID)}, instanceID, maxConcurrent).Int()
	if err != nil {
		return err
	}
	if res == 0 {
		return ErrAtCapacity
	}
	return nil
}

func (s *Store) PutInstance(ctx context.Context, inst *common.Instance) error {
	fields := map[string]any{
		"workload_id": inst.WorkloadID,
		"state":       string(inst.State),
		"host_id":     inst.HostID,
		"last_active": inst.LastActive,
		"guest_ip":    inst.GuestIP,
		"guest_port":  inst.GuestPort,
		"error":       inst.Error,
		"created_at":  inst.CreatedAt,
	}
	return s.rdb.HSet(ctx, instanceKey(inst.InstanceID), fields).Err()
}

func (s *Store) GetInstance(ctx context.Context, instanceID string) (*common.Instance, error) {
	res, err := s.rdb.HGetAll(ctx, instanceKey(instanceID)).Result()
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, ErrNotFound
	}
	lastActive, _ := strconv.ParseInt(res["last_active"], 10, 64)
	guestPort, _ := strconv.Atoi(res["guest_port"])
	createdAt, _ := strconv.ParseInt(res["created_at"], 10, 64)
	return &common.Instance{
		InstanceID: instanceID,
		WorkloadID: res["workload_id"],
		State:      common.InstanceState(res["state"]),
		HostID:     res["host_id"],
		LastActive: lastActive,
		GuestIP:    res["guest_ip"],
		GuestPort:  guestPort,
		Error:      res["error"],
		CreatedAt:  createdAt,
	}, nil
}

// casScript is the compare-and-swap primitive every instance state
// transition uses (§4.2 "Concurrency: closing the race conditions").
// Returns {1, newState} on success or {0, currentState} if the current
// state didn't match what the caller expected (currentState is "" if the
// instance doesn't exist at all).
var casScript = redis.NewScript(`
local current = redis.call('HGET', KEYS[1], 'state')
if current == false then
  return {0, ''}
end
if current ~= ARGV[1] then
  return {0, current}
end
redis.call('HSET', KEYS[1], 'state', ARGV[2])
return {1, ARGV[2]}
`)

// CAS attempts to move an instance from expected to newState. swapped is
// false if the instance was in some other state (actual tells you which) or
// didn't exist (actual == "").
func (s *Store) CAS(ctx context.Context, instanceID string, expected, newState common.InstanceState) (swapped bool, actual common.InstanceState, err error) {
	res, err := casScript.Run(ctx, s.rdb, []string{instanceKey(instanceID)}, string(expected), string(newState)).Result()
	if err != nil {
		return false, "", err
	}
	arr, ok := res.([]any)
	if !ok || len(arr) != 2 {
		return false, "", fmt.Errorf("unexpected CAS script result: %#v", res)
	}
	swappedInt, _ := arr[0].(int64)
	actualStr, _ := arr[1].(string)
	return swappedInt == 1, common.InstanceState(actualStr), nil
}

// SetInstanceState performs an unconditional state write — used only for
// DeleteInstance's "state -> DELETING" (§4.2), which proceeds regardless of
// current state (fire-and-forget) rather than being gated by a CAS
// precondition like every other transition.
func (s *Store) SetInstanceState(ctx context.Context, instanceID string, state common.InstanceState) error {
	n, err := s.rdb.Exists(ctx, instanceKey(instanceID)).Result()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return s.rdb.HSet(ctx, instanceKey(instanceID), "state", string(state)).Err()
}

// UpdateInstanceFields writes arbitrary fields on an existing instance
// record (e.g. guest_ip/guest_port/host_id after a successful boot or
// resume). Does not touch state — use CAS for that.
func (s *Store) UpdateInstanceFields(ctx context.Context, instanceID string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return s.rdb.HSet(ctx, instanceKey(instanceID), fields).Err()
}

// DeleteInstance removes the instance record, its membership in the
// workload's instance set, and its entry in the idle-reaper's ZSET.
func (s *Store) DeleteInstance(ctx context.Context, workloadID, instanceID string) error {
	pipe := s.rdb.Pipeline()
	pipe.Del(ctx, instanceKey(instanceID))
	pipe.SRem(ctx, workloadInstancesKey(workloadID), instanceID)
	pipe.ZRem(ctx, instancesDueKey, instanceID)
	_, err := pipe.Exec(ctx)
	return err
}

// TouchActivity bumps last_active and refreshes the instance's score in the
// instances_due ZSET (§4.2 "Idle-reaper loop"), in one round trip. Called on
// CreateInstance, ResumeInstance, and every heartbeat.
func (s *Store) TouchActivity(ctx context.Context, instanceID string, idleTimeoutSeconds int) error {
	now := time.Now().Unix()
	due := now + int64(idleTimeoutSeconds)
	pipe := s.rdb.Pipeline()
	pipe.HSet(ctx, instanceKey(instanceID), "last_active", now)
	pipe.ZAdd(ctx, instancesDueKey, redis.Z{Score: float64(due), Member: instanceID})
	_, err := pipe.Exec(ctx)
	return err
}

// ClearDue removes an instance from the idle-reaper's ZSET — call this on
// suspend (and delete, though DeleteInstance already does it) since a
// SUSPENDED instance has nothing left to time out.
func (s *Store) ClearDue(ctx context.Context, instanceID string) error {
	return s.rdb.ZRem(ctx, instancesDueKey, instanceID).Err()
}

// DueInstances returns instance IDs whose idle deadline has passed —
// the idle-reaper loop's per-tick query (§4.2). O(log n + k), not a full
// scan of every RUNNING instance.
func (s *Store) DueInstances(ctx context.Context) ([]string, error) {
	now := float64(time.Now().Unix())
	return s.rdb.ZRangeByScore(ctx, instancesDueKey, &redis.ZRangeBy{
		Min: "-inf",
		Max: strconv.FormatFloat(now, 'f', 0, 64),
	}).Result()
}

// --- Host registry ---

const hostsSetKey = "hosts"

func (s *Store) UpsertHost(ctx context.Context, h *common.Host) error {
	fields := map[string]any{
		"internal_addr":  h.InternalAddr,
		"status":         string(h.Status),
		"last_heartbeat": h.LastHeartbeat,
		"capacity_used":  h.CapacityUsed,
	}
	pipe := s.rdb.Pipeline()
	pipe.HSet(ctx, hostKey(h.HostID), fields)
	pipe.SAdd(ctx, hostsSetKey, h.HostID)
	_, err := pipe.Exec(ctx)
	return err
}

// ListHosts returns every registered host, healthy or not — the Controller
// filters by status itself when placing an instance (§4.2 CreateInstance
// step 5: "pick a HEALTHY host from the registry").
func (s *Store) ListHosts(ctx context.Context) ([]common.Host, error) {
	ids, err := s.rdb.SMembers(ctx, hostsSetKey).Result()
	if err != nil {
		return nil, err
	}
	hosts := make([]common.Host, 0, len(ids))
	for _, id := range ids {
		h, err := s.GetHost(ctx, id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // stale set membership from a since-removed host; skip
			}
			return nil, err
		}
		hosts = append(hosts, *h)
	}
	return hosts, nil
}

func (s *Store) GetHost(ctx context.Context, hostID string) (*common.Host, error) {
	res, err := s.rdb.HGetAll(ctx, hostKey(hostID)).Result()
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, ErrNotFound
	}
	lastHeartbeat, _ := strconv.ParseInt(res["last_heartbeat"], 10, 64)
	capacityUsed, _ := strconv.Atoi(res["capacity_used"])
	return &common.Host{
		HostID:        hostID,
		InternalAddr:  res["internal_addr"],
		Status:        common.HostStatus(res["status"]),
		LastHeartbeat: lastHeartbeat,
		CapacityUsed:  capacityUsed,
	}, nil
}

func (s *Store) AdjustHostCapacity(ctx context.Context, hostID string, delta int) error {
	return s.rdb.HIncrBy(ctx, hostKey(hostID), "capacity_used", int64(delta)).Err()
}
