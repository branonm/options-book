#!/usr/bin/env bash
# Keeps terraform.tfvars in the private, versioned state bucket, so any machine
# can deploy and a lost laptop loses nothing. The bucket name is not committed:
#
#   export TF_STATE_BUCKET=<the state bucket>
#   infra/gcp/tfvars.sh pull    # bucket -> infra/gcp/terraform.tfvars (gitignored)
#   infra/gcp/tfvars.sh push    # infra/gcp/terraform.tfvars -> bucket (a new version)
#
# The file holds no secret, but it names the project, billing account and owner,
# so it stays out of the repo. `image` is left out of it: pass it per deploy,
#   terraform apply -var "image=<registry>/options-book-mcp:<short sha>"
set -euo pipefail
: "${TF_STATE_BUCKET:?set TF_STATE_BUCKET to the state bucket name}"
dir="$(cd "$(dirname "$0")" && pwd)"
obj="gs://${TF_STATE_BUCKET}/terraform/terraform.tfvars"
case "${1:-}" in
  pull) gcloud storage cp "$obj" "$dir/terraform.tfvars" ;;
  push) gcloud storage cp "$dir/terraform.tfvars" "$obj" ;;
  *) echo "usage: $0 pull|push" >&2; exit 2 ;;
esac
