# Tier 3 host — Terraform

Manages the GCE instance used for Tier 3 (real GCP) validation of the
Firecracker microVM platform. The instance (`firecracker-experiment`,
project `container-agents-demo`, zone `us-west1-b`) already existed and was
imported into this config rather than recreated — `terraform plan` shows no
diff against it.

## Prerequisites

- `gcloud auth login --update-adc` (application default credentials — this
  is what the `google` provider authenticates with)
- `terraform init` (already done once; re-run if providers/backend change)

## Usual workflow

```bash
cd infra/terraform
terraform plan     # should show "No changes" unless you've edited compute.tf
```

Only run `terraform apply` after reviewing a plan that isn't empty — this
manages a real, billed GCE instance.

To start/stop the instance itself (not a Terraform-managed property), use
`gcloud` directly rather than Terraform:

```bash
gcloud compute instances start firecracker-experiment --zone=us-west1-b
gcloud compute instances stop  firecracker-experiment --zone=us-west1-b
```

SSH:

```bash
terraform output ssh_command   # prints the exact gcloud compute ssh command
```

## What's deliberately not managed here

- **`ssh-keys` metadata** — written transiently by `gcloud compute ssh`;
  `lifecycle.ignore_changes` keeps Terraform from clobbering it.
- **The `default` VPC network and its firewall rules** (`default-allow-ssh`,
  etc.) — referenced as data sources only. They're shared, project-wide
  resources; this config only owns the one instance.
- **OS-level setup** (Docker, Go toolchain, the `firecracker`/`jailer`
  binaries, a guest kernel image) — that's provisioning, not infrastructure;
  do it over SSH once the instance is up, not through Terraform.
