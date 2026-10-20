output "sut_public_ip" {
  value = google_compute_instance.node["sut"].network_interface[0].access_config[0].nat_ip
}

output "runner_public_ip" {
  value = google_compute_instance.node["runner"].network_interface[0].access_config[0].nat_ip
}

output "sut_private_ip" {
  value = local.sut_ip
}

output "ssh_user" {
  value = var.ssh_user
}

output "ssh_key_path" {
  value = abspath(local_sensitive_file.ssh_key.filename)
}

output "description" {
  value = "gcp ${var.zone}: SUT ${var.sut_machine_type}, runner ${var.runner_machine_type}"
}
