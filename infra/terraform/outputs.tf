output "instance_name" {
  value = google_compute_instance.firecracker_host.name
}

output "internal_ip" {
  value = google_compute_instance.firecracker_host.network_interface[0].network_ip
}

output "external_ip" {
  description = "Ephemeral — only populated while the instance is running."
  value       = try(google_compute_instance.firecracker_host.network_interface[0].access_config[0].nat_ip, null)
}

output "ssh_command" {
  value = "gcloud compute ssh ${google_compute_instance.firecracker_host.name} --zone=${var.zone} --project=${var.project_id}"
}
