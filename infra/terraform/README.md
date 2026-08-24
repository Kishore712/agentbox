# Tier 3 hosts — Terraform

Manages two GCE instances in the same VPC, project `container-agents-demo`,
zone `us-west1-b`:

- **`firecracker-experiment`** — the Firecracker/KVM host. Runs the Host
  Agent (design doc §4.3). Already existed and was imported into this
  config rather than recreated.
- **`control-plane`** — runs the Controller and REST API Service (§4.1/§4.2).
  No KVM involved; a separate, cheaper VM specifically so the Controller →
  Host Agent and REST API Service → Host Agent calls exercise a real
  cross-machine VPC path instead of localhost. Same VPC/subnet as the
  Firecracker host, so no VPN or Serverless VPC Access connector is needed
  — see the design doc's networking discussion for why that's sufficient
  at this scale and what changes if the Host Agent fleet grows.

`terraform plan` should show no changes unless you've actually edited a
`.tf` file.

## Prerequisites

- **A service account key**, not personal ADC. `baburadh@usc.edu` is a
  Google Workspace (EDU) account whose org policy silently drops the
  `cloud-platform` OAuth scope from personal `gcloud auth
  application-default login` grants — Terraform gets "insufficient
  authentication scopes" no matter how that flow is retried. The fix in use:
  a dedicated service account, `terraform-provisioner@container-agents-demo.iam.gserviceaccount.com`,
  with `roles/compute.admin` on the project and `roles/iam.serviceAccountUser`
  on `877566342501-compute@developer.gserviceaccount.com` (needed because
  both managed instances attach that service account to themselves). Its key
  lives at `~/terraform-key.json` — **outside the repo, never commit it.**
  Every `terraform` command needs:
  ```bash
  export GOOGLE_APPLICATION_CREDENTIALS="$HOME/terraform-key.json"
  ```
- `terraform init` (already done once; re-run if providers/backend change)

## Usual workflow

```bash
cd infra/terraform
export GOOGLE_APPLICATION_CREDENTIALS="$HOME/terraform-key.json"
terraform plan     # should show "No changes" unless you've edited a .tf file
```

Only run `terraform apply` after reviewing a plan that isn't empty — this
manages real, billed GCE instances.

To start/stop an instance itself (not a Terraform-managed property), use
`gcloud` directly rather than Terraform:

```bash
gcloud compute instances start firecracker-experiment --zone=us-west1-b
gcloud compute instances start control-plane --zone=us-west1-b
```

SSH:

```bash
terraform output firecracker_host_ssh_command
terraform output control_plane_ssh_command
```

## What's deliberately not managed here

- **`ssh-keys` metadata** — written transiently by `gcloud compute ssh`;
  `lifecycle.ignore_changes` keeps Terraform from clobbering it.
- **The `default` VPC network and its firewall rules** (`default-allow-ssh`,
  `default-allow-internal`, etc.) — referenced as data sources only.
  They're shared, project-wide resources; this config only owns the two
  instances. `default-allow-internal` (source range `10.128.0.0/9`) already
  covers `control-plane` reaching the Host Agent's `:9000` — no new
  firewall rule was needed for that.
- **OS-level setup** (Docker, Go toolchain, the `firecracker`/`jailer`
  binaries, a guest kernel image on `firecracker-experiment`; Docker + Go
  on `control-plane`) — that's provisioning, not infrastructure; do it over
  SSH once an instance is up, not through Terraform.
- **No public port for the REST API Service.** `control-plane` only has SSH
  open externally. Opening its port to the internet is a deliberate,
  separate decision — not something to fold into infrastructure setup.
