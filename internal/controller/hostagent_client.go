package controller

import "context"

// BootVMRequest is the body of the Host Agent's POST /vm (§4.2 "B) Host
// Agent API").
type BootVMRequest struct {
	InstanceID      string   `json:"instance_id"`
	RootfsRef       string   `json:"rootfs_ref"`
	VCPUs           int      `json:"vcpus"`
	MemoryMiB       int      `json:"memory_mib"`
	EgressAllowlist []string `json:"egress_allowlist"`
}

type VMEndpoint struct {
	GuestIP   string `json:"guest_ip"`
	GuestPort int    `json:"guest_port"`
}

// HostAgentClient is the Controller's view of a single Host Agent. Defined
// as an interface so the Controller's service layer can be unit tested
// against a fake implementation without a real GCE host / KVM / Firecracker
// present — the real HTTP-backed implementation lives in
// internal/controller/hostagent_http_client.go and is only exercised by the
// integration/GCP validation phases.
type HostAgentClient interface {
	BootVM(ctx context.Context, hostAddr string, req BootVMRequest) (VMEndpoint, error)
	SuspendVM(ctx context.Context, hostAddr, instanceID string) error
	ResumeVM(ctx context.Context, hostAddr, instanceID string) (VMEndpoint, error)
	DeleteVM(ctx context.Context, hostAddr, instanceID string) error
}
