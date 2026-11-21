# Phase 5 (PLAN.md, "Phase 5 design"): the credential layer.
#
#   secrets      robinhood-oauth, alpaca-paper. Terraform creates them empty;
#                `ob cred enroll` adds the first version from the owner's
#                machine, so no credential is ever in Terraform state.
#   ob-mcp    reads the latest version of both, and nothing else in
#                Secret Manager. The server never refreshes.
#   refresher    options-book-cred-refresher: reads, adds, lists, disables and
#                destroys versions of robinhood-oauth only. No Firestore, no
#                other secret. Only it (and the owner) write robinhood-oauth.
#   job          options-book-cred-refresh: the server image running
#                `ob cred refresh robinhood`, as the refresher.
#   trigger      Cloud Scheduler runs the job daily, as options-book-cred-scheduler,
#                which can run that one job and do nothing else.
#
# Cloud Scheduler's free allowance: 3 jobs a month (confirmed by the owner from
# the pricing page, 11/17). This adds one job, the project's first (the API
# wasn't enabled before phase 5); it fits unless other projects on the same
# billing account already run Scheduler jobs.

resource "google_project_service" "cred_apis" {
  for_each           = toset(["secretmanager.googleapis.com", "cloudscheduler.googleapis.com"])
  service            = each.value
  disable_on_destroy = false
}

resource "google_secret_manager_secret" "cred" {
  for_each  = toset(["robinhood-oauth", "alpaca-paper"])
  secret_id = each.value
  replication {
    user_managed {
      replicas {
        location = var.region
      }
    }
  }
  depends_on = [google_project_service.cred_apis]
}

# ob-mcp: read the latest version of each, nothing more.
resource "google_secret_manager_secret_iam_member" "mcp_reads" {
  for_each  = google_secret_manager_secret.cred
  secret_id = each.value.id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.mcp.email}"
}

resource "google_service_account" "refresher" {
  depends_on   = [google_project_service.apis]
  account_id   = "options-book-cred-refresher"
  display_name = "${var.name_prefix} credential refresher (robinhood-oauth only)"
}

# Exactly what cred.Rotate does: read the latest version, add one, list,
# disable the previous, destroy older. Granted on robinhood-oauth only.
resource "google_project_iam_custom_role" "refresher" {
  depends_on  = [google_project_service.apis]
  role_id     = "optionsBookCredRefresher"
  title       = "${var.name_prefix} credential refresher"
  description = "Rotate versions of one secret: access, add, list, disable, destroy. Granted on robinhood-oauth only."
  permissions = [
    "secretmanager.versions.access",
    "secretmanager.versions.add",
    "secretmanager.versions.list",
    "secretmanager.versions.get",
    "secretmanager.versions.disable",
    "secretmanager.versions.destroy",
  ]
}

resource "google_secret_manager_secret_iam_member" "refresher_rotates" {
  secret_id = google_secret_manager_secret.cred["robinhood-oauth"].id
  role      = google_project_iam_custom_role.refresher.id
  member    = "serviceAccount:${google_service_account.refresher.email}"
}

resource "google_cloud_run_v2_job" "refresh" {
  name                = "options-book-cred-refresh"
  location            = var.region
  deletion_protection = false
  template {
    task_count = 1
    template {
      service_account = google_service_account.refresher.email
      max_retries     = 1
      # Jobs run in the gen2 environment, which needs at least 512Mi with a full CPU
      # (a 256Mi job was refused at apply, 11/17). It runs seconds a day.
      timeout = "120s"
      containers {
        image   = var.image
        command = ["/ob"]
        args    = ["cred", "refresh", "robinhood"]
        env {
          name  = "WHEEL_PROJECT"
          value = var.project_id
        }
        resources {
          limits = { cpu = "1", memory = "512Mi" }
        }
      }
    }
  }
  depends_on = [google_project_service.apis, google_secret_manager_secret_iam_member.refresher_rotates]
}

resource "google_service_account" "scheduler" {
  depends_on   = [google_project_service.apis]
  account_id   = "options-book-cred-scheduler"
  display_name = "${var.name_prefix}: runs the credential refresh job"
}

resource "google_cloud_run_v2_job_iam_member" "scheduler_runs_refresh" {
  name     = google_cloud_run_v2_job.refresh.name
  location = var.region
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_service_account.scheduler.email}"
}

# Daily. The job refreshes only when under two days are left (cred.RefreshWindow),
# so most runs change nothing; an ~8.8-day token is refreshed about every 7 days.
resource "google_cloud_scheduler_job" "refresh_daily" {
  name      = "options-book-cred-refresh-daily"
  region    = var.region
  schedule  = "0 13 * * *"
  time_zone = "Etc/UTC"
  http_target {
    http_method = "POST"
    uri         = "https://run.googleapis.com/v2/projects/${var.project_id}/locations/${var.region}/jobs/${google_cloud_run_v2_job.refresh.name}:run"
    oauth_token {
      service_account_email = google_service_account.scheduler.email
    }
  }
  depends_on = [google_project_service.cred_apis, google_cloud_run_v2_job_iam_member.scheduler_runs_refresh]
}

output "refresher_service_account" {
  value = google_service_account.refresher.email
}
