# The Controller and REST API Service (design doc §4.1/§4.2) run here —
# deliberately a separate VM from firecracker-experiment, not because
# either needs its own hardware, but to actually exercise the real
# cross-machine network path (this VM → the Host Agent's VPC-internal
# address, :9000) that production would use, rather than papering over it
# with localhost. Same VPC/subnet as the Firecracker host — no VPN or
# Serverless VPC Access connector needed, just ordinary GCP VPC routing
# (GCP VPCs are global resources with regional subnets, so this scales to
# more regions without re-architecting network access).
#
# Neither Controller nor the REST API Service touches /dev/kvm, so this
# instance intentionally has no advanced_machine_features block and no
# nested-virtualization-capable machine type requirement.

resource "google_compute_instance" "control_plane" {
  name         = var.control_plane_instance_name
  zone         = var.zone
  machine_type = var.control_plane_machine_type

  boot_disk {
    auto_delete = true
    initialize_params {
      image = "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64"
      size  = 50 # Controller runs the real Image Builder (Docker pull/export + ext4 rootfs staging)
      type  = "pd-standard"
    }
  }

  network_interface {
    network    = data.google_compute_network.default.self_link
    subnetwork = data.google_compute_subnetwork.default.self_link

    # Ephemeral external IP for SSH only — no service port is exposed
    # publicly. Controller/REST API Service reach the Host Agent, and the
    # Host Agent's firewall (default-allow-internal, 10.128.0.0/9) already
    # permits that without any additional rule.
    access_config {
      network_tier = "PREMIUM"
    }
  }

  shielded_instance_config {
    enable_secure_boot          = true
    enable_vtpm                 = true
    enable_integrity_monitoring = true
  }

  # cloud-platform here is the OAuth *scope* — a ceiling on what this VM's
  # attached identity can ever request a token for. Actual permissions are
  # still governed entirely by IAM roles granted to that identity (below,
  # via google_artifact_registry_repository_iam_member), not by the scope
  # itself — this is the standard modern pattern (broad scope, narrow IAM),
  # and it's what lets this VM authenticate to APIs like Artifact Registry
  # using its own attached identity via the metadata server, with no static
  # key ever touching disk.
  service_account {
    email = "877566342501-compute@developer.gserviceaccount.com"
    scopes = [
      "https://www.googleapis.com/auth/cloud-platform",
    ]
  }

  scheduling {
    automatic_restart   = true
    on_host_maintenance = "MIGRATE"
    preemptible         = false
  }

  can_ip_forward      = false
  deletion_protection = false

  # Required by the provider before it will touch service_account.scopes
  # on a running instance — GCP itself demands the instance be stopped
  # for that specific change (verified directly against the API before
  # this was set, not assumed); this just lets Terraform actually perform
  # the stop/update/restart instead of refusing.
  allow_stopping_for_update = true

  lifecycle {
    ignore_changes = [metadata] # same rationale as firecracker_host — see compute.tf
  }
}
