// Package common holds types shared by the Controller, Host Agent, and REST
// API Service. It mirrors the Redis schema and API contracts defined in
// Docs/agent-sandbox-platform-design-v3.md §4.2/§4.4.
package common

// WorkloadStatus is the readiness of a Workload's golden rootfs build.
type WorkloadStatus string

const (
	WorkloadProvisioning WorkloadStatus = "PROVISIONING"
	WorkloadReady        WorkloadStatus = "READY"
	WorkloadFailed       WorkloadStatus = "FAILED"
)

// InstanceState is the microVM lifecycle state machine (design doc §4.2).
type InstanceState string

const (
	InstanceCreating   InstanceState = "CREATING"
	InstanceRunning    InstanceState = "RUNNING"
	InstanceSuspending InstanceState = "SUSPENDING"
	InstanceSuspended  InstanceState = "SUSPENDED"
	InstanceResuming   InstanceState = "RESUMING"
	InstanceFailed     InstanceState = "FAILED"
	InstanceDeleting   InstanceState = "DELETING"
)

// HostStatus reflects whether a Host Agent is answering health checks.
type HostStatus string

const (
	HostHealthy   HostStatus = "HEALTHY"
	HostUnhealthy HostStatus = "UNHEALTHY"
)

// Workload is the control-plane record for a registered image + config.
// Redis key: workload:{workload_id}
type Workload struct {
	WorkloadID             string         `json:"workload_id"`
	Name                   string         `json:"name"`
	ImageRef               string         `json:"image_ref"`
	Status                 WorkloadStatus `json:"status"`
	RootfsRef              string         `json:"-"` // internal-only, never serialized in any API response
	IdleTimeoutSeconds     int            `json:"idle_timeout_seconds"`
	EgressAllowlist        []string       `json:"egress_allowlist"`
	VCPUs                  int            `json:"vcpus"`
	MemoryMiB              int            `json:"memory_mib"`
	MaxConcurrentInstances int            `json:"max_concurrent_instances"`
	CreatedAt              int64          `json:"created_at"`
}

// Instance is the actual microVM — one dedicated Firecracker process, one
// durable home volume. Redis key: instance:{instance_id}
type Instance struct {
	InstanceID string        `json:"instance_id"`
	WorkloadID string        `json:"workload_id"`
	State      InstanceState `json:"state"`
	HostID     string        `json:"host_id"`
	LastActive int64         `json:"last_active"`
	GuestIP    string        `json:"guest_ip"`
	GuestPort  int           `json:"guest_port"`
	Error      string        `json:"error,omitempty"`
	CreatedAt  int64         `json:"created_at"`
}

// Host is one registered Host Agent. Redis key: host:{host_id}
type Host struct {
	HostID        string     `json:"host_id"`
	InternalAddr  string     `json:"internal_addr"`
	Status        HostStatus `json:"status"`
	LastHeartbeat int64      `json:"last_heartbeat"`
	CapacityUsed  int        `json:"capacity_used"`
}
