# Phase 1 spike infrastructure (the storage design).
#
# Creates: the APIs, a Firestore database, an Artifact Registry repository,
# the wheel-mcp service account with an insert-only custom role, the Cloud
# Run service, and a $1 billing budget.
#
# Not created here (done by hand, see spike/gcpping/README.md): the project
# itself, the OAuth consent screen and the OAuth client Claude signs in with.

terraform {
  required_version = ">= 1.6"
  # State lives in a private, versioned Cloud Storage bucket. The bucket name is
  # not committed; pass it at init:
  #   terraform init -backend-config="bucket=<bucket>" -backend-config="prefix=terraform/state"
  backend "gcs" {}
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }
}

provider "google" {
  project               = var.project_id
  region                = var.region
  billing_project       = var.project_id
  user_project_override = true
}

data "google_project" "this" {}

# An organization's iam.automaticIamGrantsForDefaultServiceAccounts policy
# blocks Google's usual automatic grant of roles/editor to the Compute
# Engine default service account, which Cloud Build runs as. Grant back just
# what gcloud builds submit actually needs: reading the source it uploads to
# its staging bucket (storage.objectViewer), writing build logs to Cloud
# Logging since cloudbuild.yaml sets logging: CLOUD_LOGGING_ONLY
# (logging.logWriter), and pushing the built image to Artifact Registry
# (artifactregistry.writer). Not needed in a project with no such org policy.
resource "google_project_iam_member" "cloudbuild_default_sa" {
  for_each = toset(["roles/storage.objectViewer", "roles/logging.logWriter", "roles/artifactregistry.writer"])
  project  = var.project_id
  role     = each.value
  member   = "serviceAccount:${data.google_project.this.number}-compute@developer.gserviceaccount.com"
}

locals {
  apis = concat(var.enable_firestore ? ["firestore.googleapis.com"] : [], [
    "run.googleapis.com",
    "storage.googleapis.com",
    "artifactregistry.googleapis.com",
    "cloudbuild.googleapis.com",
    "iam.googleapis.com",
    "billingbudgets.googleapis.com",
  ])
  # Cloud Run's deterministic URL, so the server can be told its own address
  # in the same apply that creates it.
  public_url = "https://${var.service_name}-${data.google_project.this.number}.${var.region}.run.app"
}

resource "google_project_service" "apis" {
  for_each           = toset(local.apis)
  service            = each.value
  disable_on_destroy = false
}

resource "google_firestore_database" "main" {
  count                   = var.enable_firestore ? 1 : 0
  name                    = "(default)"
  location_id             = var.region
  type                    = "FIRESTORE_NATIVE"
  delete_protection_state = "DELETE_PROTECTION_ENABLED"
  deletion_policy         = "ABANDON"
  depends_on              = [google_project_service.apis]
}

resource "google_artifact_registry_repository" "images" {
  repository_id = var.name_prefix
  location      = var.region
  format        = "DOCKER"
  # Stay inside the 0.5 GB free allowance.
  cleanup_policy_dry_run = false
  cleanup_policies {
    id     = "keep-recent"
    action = "KEEP"
    most_recent_versions {
      keep_count = 3
    }
  }
  cleanup_policies {
    id     = "delete-old"
    action = "DELETE"
    condition {
      older_than = "604800s"
    }
  }
  depends_on = [google_project_service.apis]
}

resource "google_service_account" "mcp" {
  depends_on   = [google_project_service.apis]
  account_id   = "wheel-mcp"
  display_name = "${var.name_prefix} MCP server"
}

# The append-only guarantee: create, read and list, never update or delete.
resource "google_project_iam_custom_role" "insert_only" {
  depends_on  = [google_project_service.apis]
  count       = var.enable_firestore ? 1 : 0
  role_id     = "wheelMcpInsertOnly"
  title       = "${var.name_prefix} insert-only Firestore access"
  description = "Create and read Firestore documents; no update, no delete."
  permissions = [
    "datastore.databases.get",
    "datastore.entities.get",
    "datastore.entities.list",
    "datastore.entities.create",
  ]
}

resource "google_project_iam_member" "mcp_insert_only" {
  count   = var.enable_firestore ? 1 : 0
  project = var.project_id
  role    = google_project_iam_custom_role.insert_only[0].id
  member  = "serviceAccount:${google_service_account.mcp.email}"
}

resource "google_cloud_run_v2_service" "mcp" {
  name                = var.service_name
  location            = var.region
  ingress             = "INGRESS_TRAFFIC_ALL"
  deletion_protection = false

  template {
    service_account = google_service_account.mcp.email
    scaling {
      min_instance_count = 0
      max_instance_count = 2
    }
    max_instance_request_concurrency = 20
    timeout                          = "30s"
    containers {
      image = var.image
      resources {
        limits   = { cpu = "1", memory = "256Mi" }
        cpu_idle = true
      }
      env {
        name  = "OAUTH_CLIENT_ID"
        value = var.oauth_client_id
      }
      env {
        name  = "ALLOWED_EMAIL"
        value = var.allowed_email
      }
      env {
        name  = "PUBLIC_URL"
        value = local.public_url
      }
      env {
        name  = "PROJECT_ID"
        value = var.project_id
      }
      dynamic "env" {
        for_each = var.config_bucket == "" ? [] : [var.config_bucket]
        content {
          name  = "CONFIG_BUCKET"
          value = env.value
        }
      }
      # Which store the server reads and writes. Firestore (no STORE variable) is
      # the default; "cloudsql" is the switch in docs/cloud-sql-design.md, step 5.
      dynamic "env" {
        for_each = var.store_backend == "cloudsql" ? {
          STORE             = "cloudsql"
          CLOUDSQL_INSTANCE = google_sql_database_instance.main[0].connection_name
          CLOUDSQL_DATABASE = var.cloudsql_database
          CLOUDSQL_USER     = local.server_db_user
        } : {}
        content {
          name  = env.key
          value = env.value
        }
      }
    }
  }
  depends_on = [google_project_service.apis, google_project_iam_member.mcp_insert_only, google_storage_bucket_iam_member.config_reader]

  # Google fills in a service-level scaling block with its defaults (min 0,
  # manual 0) after every apply, and Terraform would clear it every time, a
  # permanent diff that hides real ones. Our scaling is template.scaling above.
  lifecycle {
    ignore_changes = [scaling]
    precondition {
      condition     = var.store_backend != "cloudsql" || var.enable_cloudsql
      error_message = "store_backend = \"cloudsql\" needs enable_cloudsql = true."
    }
    precondition {
      condition     = var.store_backend != "firestore" || var.enable_firestore
      error_message = "store_backend = \"firestore\" needs enable_firestore = true."
    }
  }
}

# Claude can't present a Google identity token, so the service is open at
# the network level; the server itself checks the OAuth token, the account
# and Anthropic's address range on every request.
resource "google_cloud_run_v2_service_iam_member" "public" {
  name     = google_cloud_run_v2_service.mcp.name
  location = var.region
  role     = "roles/run.invoker"
  member   = "allUsers"
}

resource "google_billing_budget" "guard" {
  billing_account = var.billing_account
  display_name    = "${var.name_prefix} guard"
  budget_filter {
    projects = ["projects/${data.google_project.this.number}"]
  }
  amount {
    specified_amount {
      currency_code = "USD"
      units         = var.enable_cloudsql ? var.cloudsql_budget_usd : "1"
    }
  }
  threshold_rules { threshold_percent = 0.5 }
  threshold_rules { threshold_percent = 1.0 }
  depends_on = [google_project_service.apis]
}

output "mcp_url" {
  description = "Enter this in Claude as the custom connector URL."
  value       = "${local.public_url}/mcp"
}

output "service_account" {
  value = google_service_account.mcp.email
}

output "image_repository" {
  value = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.images.repository_id}"
}

# These three gained a count (enable_firestore); keep existing state attached.
moved {
  from = google_firestore_database.main
  to   = google_firestore_database.main[0]
}

moved {
  from = google_project_iam_custom_role.insert_only
  to   = google_project_iam_custom_role.insert_only[0]
}

moved {
  from = google_project_iam_member.mcp_insert_only
  to   = google_project_iam_member.mcp_insert_only[0]
}
