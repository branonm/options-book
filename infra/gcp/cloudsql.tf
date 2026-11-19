# The Cloud SQL (PostgreSQL) store, an alternative to Firestore. See
# docs/cloud-sql-design.md.
#
# Everything here is off by default: with enable_cloudsql = false (the default)
# applying this file creates, changes and destroys nothing. Turn it on only when
# you mean to run the migration, and read the runbook in the design doc first.
#
# What this creates, and what it deliberately leaves to a person:
#   - the instance (public address, no authorised networks; reached only through
#     the Cloud SQL Go connector with IAM database authentication, so no password
#     exists anywhere),
#   - the database,
#   - an IAM database user for the server's service account, and one for the
#     owner's Google account (who creates the schema),
#   - roles/cloudsql.client and roles/cloudsql.instanceUser for both.
# It does NOT create the table, the append-only trigger or the server's SELECT
# and INSERT grant: `ob db migrate` does that, run by the owner. Terraform never
# holds a database password.

locals {
  cloudsql_count = var.enable_cloudsql ? 1 : 0
  # Cloud SQL names a service account's IAM database user without this suffix.
  server_db_user = trimsuffix(google_service_account.mcp.email, ".gserviceaccount.com")
}

resource "google_project_service" "sqladmin" {
  count              = local.cloudsql_count
  service            = "sqladmin.googleapis.com"
  disable_on_destroy = false
}

resource "google_sql_database_instance" "main" {
  count               = local.cloudsql_count
  name                = "options-book-pg"
  region              = var.region
  database_version    = "POSTGRES_16"
  deletion_protection = true

  settings {
    edition                     = "ENTERPRISE"
    tier                        = var.cloudsql_tier
    availability_type           = "ZONAL"
    disk_type                   = "PD_SSD"
    disk_size                   = 10
    disk_autoresize             = true
    deletion_protection_enabled = true

    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = false
      backup_retention_settings {
        retained_backups = 7
      }
    }

    ip_configuration {
      ipv4_enabled = true
      ssl_mode     = "ENCRYPTED_ONLY"
      # No authorized_networks: nothing reaches the instance without the IAM role.
    }

    database_flags {
      name  = "cloudsql.iam_authentication"
      value = "on"
    }
  }

  depends_on = [google_project_service.sqladmin]
}

resource "google_sql_database" "main" {
  count    = local.cloudsql_count
  name     = var.cloudsql_database
  instance = google_sql_database_instance.main[0].name
}

resource "google_sql_user" "server" {
  count    = local.cloudsql_count
  instance = google_sql_database_instance.main[0].name
  name     = local.server_db_user
  type     = "CLOUD_IAM_SERVICE_ACCOUNT"
}

resource "google_sql_user" "owner" {
  count    = local.cloudsql_count
  instance = google_sql_database_instance.main[0].name
  name     = var.allowed_email
  type     = "CLOUD_IAM_USER"
}

resource "google_project_iam_member" "server_cloudsql" {
  for_each = var.enable_cloudsql ? toset(["roles/cloudsql.client", "roles/cloudsql.instanceUser"]) : toset([])
  project  = var.project_id
  role     = each.value
  member   = "serviceAccount:${google_service_account.mcp.email}"
}

resource "google_project_iam_member" "owner_cloudsql" {
  for_each = var.enable_cloudsql ? toset(["roles/cloudsql.client", "roles/cloudsql.instanceUser"]) : toset([])
  project  = var.project_id
  role     = each.value
  member   = "user:${var.allowed_email}"
}

output "cloudsql_connection_name" {
  description = "PROJECT:REGION:INSTANCE, for `ob db --cloudsql <this>/<database>/<user>`."
  value       = var.enable_cloudsql ? google_sql_database_instance.main[0].connection_name : null
}

output "cloudsql_server_user" {
  description = "The server's IAM database user: pass it to `ob db migrate --server-role`."
  value       = var.enable_cloudsql ? local.server_db_user : null
}
