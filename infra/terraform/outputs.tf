output "firecracker_host_name" {
  value = google_compute_instance.firecracker_host.name
}

output "firecracker_host_internal_ip" {
  value = google_compute_instance.firecracker_host.network_interface[0].network_ip
}

output "firecracker_host_external_ip" {
  description = "Ephemeral — only populated while the instance is running."
  value       = try(google_compute_instance.firecracker_host.network_interface[0].access_config[0].nat_ip, null)
}

output "firecracker_host_ssh_command" {
  value = "gcloud compute ssh ${google_compute_instance.firecracker_host.name} --zone=${var.zone} --project=${var.project_id}"
}

output "control_plane_name" {
  value = google_compute_instance.control_plane.name
}

output "control_plane_internal_ip" {
  description = "This is what the control-plane VM uses to reach the Host Agent's data-plane/control-plane API (:9000) — and what HOST_AGENTS on the Controller and CONTROLLER_URL on the REST API Service resolve to, in reverse."
  value       = google_compute_instance.control_plane.network_interface[0].network_ip
}

output "control_plane_external_ip" {
  description = "Ephemeral — only populated while the instance is running."
  value       = try(google_compute_instance.control_plane.network_interface[0].access_config[0].nat_ip, null)
}

output "control_plane_ssh_command" {
  value = "gcloud compute ssh ${google_compute_instance.control_plane.name} --zone=${var.zone} --project=${var.project_id}"
}
