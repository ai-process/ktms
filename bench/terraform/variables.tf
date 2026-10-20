variable "project" {
  description = "GCP project to create the stand in"
  type        = string
}

variable "region" {
  description = "GCP region"
  type        = string
  default     = "europe-west3"
}

variable "zone" {
  description = "GCP zone; both VMs share it to keep LAN latency minimal"
  type        = string
  default     = "europe-west3-a"
}

variable "prefix" {
  description = "Name prefix for every resource of this stand"
  type        = string
  default     = "ktms-bench"
}

# Unlike E2, C3 vCPUs are not overcommitted and run on one CPU generation,
# which keeps results reproducible between runs.
variable "sut_machine_type" {
  description = "System under test: gateways, Postgres, Redis, mock upstream"
  type        = string
  default     = "c3-standard-4"
}

variable "runner_machine_type" {
  description = "Load generator running k6"
  type        = string
  default     = "c3-standard-4"
}

variable "subnet_cidr" {
  description = "Subnet for the stand; SUT gets .10, runner .20"
  type        = string
  default     = "10.77.1.0/24"
}

variable "ssh_allowed_cidrs" {
  description = "Sources allowed to SSH into the stand; run_cloud.sh passes the caller's /32"
  type        = list(string)
}

variable "ssh_user" {
  description = "Login user created by cloud-init"
  type        = string
  default     = "bench"
}
