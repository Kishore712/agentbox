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

	// HasRootfs and PushRootfs implement §4.6's placement-locality fix —
	// called from CreateInstance before BootVM, so a workload's golden
	// rootfs (built by the Image Builder, which runs inside the Controller
	// process) actually exists on whichever host is about to boot an
	// instance of it. rootfsRef is the exact path from the workload record
	// — see Store's rootfs_ref field.
	HasRootfs(ctx context.Context, hostAddr, rootfsRef string) (bool, error)
	PushRootfs(ctx context.Context, hostAddr, rootfsRef string) error
}
