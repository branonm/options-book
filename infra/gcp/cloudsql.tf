# The Cloud SQL (PostgreSQL) store, the only store. See docs/cloud-sql-design.md
# for the design and the runbook.
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
  # Cloud SQL names a service account's IAM database user without this suffix.
  server_db_user = trimsuffix(google_service_account.mcp.email, ".gserviceaccount.com")
}

resource "google_project_service" "sqladmin" {
  service            = "sqladmin.googleapis.com"
  disable_on_destroy = false
}

resource "google_sql_database_instance" "main" {
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
  name     = var.cloudsql_database
  instance = google_sql_database_instance.main.name
}

resource "google_sql_user" "server" {
  instance = google_sql_database_instance.main.name
  name     = local.server_db_user
  type     = "CLOUD_IAM_SERVICE_ACCOUNT"
}

resource "google_sql_user" "owner" {
  instance = google_sql_database_instance.main.name
  name     = var.allowed_email
  type     = "CLOUD_IAM_USER"
}

resource "google_project_iam_member" "server_cloudsql" {
  for_each = toset(["roles/cloudsql.client", "roles/cloudsql.instanceUser"])
  project  = var.project_id
  role     = each.value
  member   = "serviceAccount:${google_service_account.mcp.email}"
}

resource "google_project_iam_member" "owner_cloudsql" {
  for_each = toset(["roles/cloudsql.client", "roles/cloudsql.instanceUser"])
  project  = var.project_id
  role     = each.value
  member   = "user:${var.allowed_email}"
}

output "cloudsql_connection_name" {
  description = "PROJECT:REGION:INSTANCE, for `ob db --cloudsql <this>/<database>/<user>`."
  value       = google_sql_database_instance.main.connection_name
}

output "cloudsql_server_user" {
  description = "The server's IAM database user: pass it to `ob db migrate --server-role`."
  value       = local.server_db_user
}

# These five lost their count (enable_cloudsql); keep existing state attached,
# so the plan does not replace the database.
moved {
  from = google_project_service.sqladmin[0]
  to   = google_project_service.sqladmin
}

moved {
  from = google_sql_database_instance.main[0]
  to   = google_sql_database_instance.main
}

moved {
  from = google_sql_database.main[0]
  to   = google_sql_database.main
}

moved {
  from = google_sql_user.server[0]
  to   = google_sql_user.server
}

moved {
  from = google_sql_user.owner[0]
  to   = google_sql_user.owner
}
