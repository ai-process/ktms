terraform {
  required_version = ">= 1.5.0"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 7.0"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.0"
    }
    local = {
      source  = "hashicorp/local"
      version = "~> 2.5"
    }
  }
}

# Credentials come from Application Default Credentials (gcloud auth application-default login).
provider "google" {
  project = var.project
  region  = var.region
  zone    = var.zone
}

locals {
  labels  = { "managed-by" = "ktms-bench", "bench" = var.prefix }
  tag     = var.prefix
  sut_ip  = cidrhost(var.subnet_cidr, 10)
  run_ip  = cidrhost(var.subnet_cidr, 20)
  ssh_key = "${path.module}/.ssh/id_ed25519"
}

# Throwaway key per stand, injected only into these VMs; nothing outlives destroy.
resource "tls_private_key" "bench" {
  algorithm = "ED25519"
}

resource "local_sensitive_file" "ssh_key" {
  content         = tls_private_key.bench.private_key_openssh
  filename        = local.ssh_key
  file_permission = "0600"
}

resource "google_compute_network" "bench" {
  name                    = "${var.prefix}-net"
  auto_create_subnetworks = false
}

resource "google_compute_subnetwork" "bench" {
  name          = "${var.prefix}-subnet"
  network       = google_compute_network.bench.id
  region        = var.region
  ip_cidr_range = var.subnet_cidr
}

resource "google_compute_firewall" "ssh" {
  name          = "${var.prefix}-ssh"
  network       = google_compute_network.bench.id
  source_ranges = var.ssh_allowed_cidrs
  target_tags   = [local.tag]

  allow {
    protocol = "tcp"
    ports    = ["22"]
  }
}

# Gateway ports are reachable only from inside the stand's subnet.
resource "google_compute_firewall" "internal" {
  name          = "${var.prefix}-internal"
  network       = google_compute_network.bench.id
  source_ranges = [var.subnet_cidr]
  target_tags   = [local.tag]

  allow {
    protocol = "tcp"
  }
  allow {
    protocol = "icmp"
  }
}

resource "google_compute_instance" "node" {
  for_each = {
    sut    = { ip = local.sut_ip, type = var.sut_machine_type, init = "sut.yaml" }
    runner = { ip = local.run_ip, type = var.runner_machine_type, init = "runner.yaml" }
  }

  name         = "${var.prefix}-${each.key}"
  machine_type = each.value.type
  zone         = var.zone
  tags         = [local.tag]
  labels       = merge(local.labels, { role = each.key })

  boot_disk {
    initialize_params {
      image = "ubuntu-os-cloud/ubuntu-2404-lts-amd64"
      size  = 30
      type  = "pd-balanced"
    }
  }

  network_interface {
    subnetwork = google_compute_subnetwork.bench.id
    network_ip = each.value.ip
    access_config {}
  }

  metadata = {
    block-project-ssh-keys = "true"
    user-data = templatefile("${path.module}/cloud-init/${each.value.init}", {
      ssh_user       = var.ssh_user
      ssh_public_key = trimspace(tls_private_key.bench.public_key_openssh)
    })
  }

  scheduling {
    provisioning_model = "STANDARD"
    automatic_restart  = false
  }
}
