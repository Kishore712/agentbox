variable "project_id" {
  description = "GCP project the Tier 3 host lives in."
  type        = string
  default     = "container-agents-demo"
}

variable "region" {
  description = "Region for the subnetwork reference."
  type        = string
  default     = "us-west1"
}

variable "zone" {
  description = "Zone for the Firecracker host — must support nested virtualization on the chosen machine type."
  type        = string
  default     = "us-west1-b"
}

variable "instance_name" {
  description = "Name of the Firecracker host instance."
  type        = string
  default     = "firecracker-experiment"
}

variable "machine_type" {
  description = "Machine type. Must be an N2/N2D/C2/C2D family (or similar) — nested virtualization (advanced_machine_features) isn't supported on every family."
  type        = string
  default     = "n2-standard-4"
}

variable "control_plane_instance_name" {
  description = "Name of the VM running Controller + REST API Service (design doc §4.1/§4.2) — no KVM involved, deliberately separate from the Firecracker host."
  type        = string
  default     = "control-plane"
}

variable "control_plane_machine_type" {
  description = "Machine type for the control-plane VM. No nested virtualization needed — any family works; picked for cost, not capability."
  type        = string
  default     = "e2-small"
}
