# Imports and manages the existing "firecracker-experiment" GCE instance —
# not created by this config. It already had nested virtualization enabled
# and the right shielded-VM settings for running Firecracker/KVM; this
# config exists to bring it under version control and make its
# configuration reproducible, not to replace it. See infra/terraform/README.md
# for the import procedure.

data "google_compute_network" "default" {
  name = "default"
}

data "google_compute_subnetwork" "default" {
  name   = "default"
  region = var.region
}

resource "google_compute_instance" "firecracker_host" {
  name         = var.instance_name
  zone         = var.zone
  machine_type = var.machine_type

  # Firecracker needs /dev/kvm, which requires nested virtualization since
  # this instance is itself a VM (design doc §4.3/§4.7 — the whole platform
  # runs Firecracker microVMs inside a GCE guest).
  advanced_machine_features {
    enable_nested_virtualization = true
  }

  boot_disk {
    auto_delete = true
    initialize_params {
      image = "projects/ubuntu-os-cloud/global/images/ubuntu-2404-noble-amd64-v20260820"
      size  = 200
      type  = "pd-standard"
    }
  }

  network_interface {
    network    = data.google_compute_network.default.self_link
    subnetwork = data.google_compute_subnetwork.default.self_link

    # Ephemeral external IP — matches how the instance is already
    # configured. Needed for `gcloud compute ssh` / package installs;
    # nothing on this box should be reachable beyond SSH (see the
    # default-allow-ssh firewall rule already on this network).
    access_config {
      network_tier = "PREMIUM"
    }
  }

  shielded_instance_config {
    enable_secure_boot          = false
    enable_vtpm                 = true
    enable_integrity_monitoring = true
  }

  # Matches the Console UI's "Allow default access" preset, which is what
  # this instance was actually created with — listed in full so importing
  # it doesn't silently narrow its permissions.
  service_account {
    email = "877566342501-compute@developer.gserviceaccount.com"
    scopes = [
      "https://www.googleapis.com/auth/devstorage.read_only",
      "https://www.googleapis.com/auth/logging.write",
      "https://www.googleapis.com/auth/monitoring.write",
      "https://www.googleapis.com/auth/pubsub",
      "https://www.googleapis.com/auth/service.management.readonly",
      "https://www.googleapis.com/auth/servicecontrol",
      "https://www.googleapis.com/auth/trace.append",
    ]
  }

  scheduling {
    automatic_restart   = true
    on_host_maintenance = "MIGRATE"
    preemptible         = false
  }

  can_ip_forward      = false
  deletion_protection = false

  # ssh-keys metadata is written transiently by `gcloud compute ssh` (OS
  # Login-style, expires on its own) — intentionally not managed here so
  # Terraform never clobbers it out from under an active SSH session.
  lifecycle {
    ignore_changes = [metadata]
  }
}
