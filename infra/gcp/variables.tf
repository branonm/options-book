variable "project_id" {
  description = "The GCP project created for options-book."
  type        = string
}

variable "billing_account" {
  description = "Billing account id (XXXXXX-XXXXXX-XXXXXX) the project is linked to; used for the $1 budget."
  type        = string
}

variable "region" {
  type    = string
  default = "us-east1"
}

variable "service_name" {
  type    = string
  default = "wheel-mcp"
}

variable "image" {
  description = "Container image for the server. The first apply uses a public placeholder; see the README."
  type        = string
  default     = "us-docker.pkg.dev/cloudrun/container/hello"
}

variable "oauth_client_id" {
  description = "Client id of the Google OAuth client Claude signs in with."
  type        = string
}

variable "allowed_email" {
  description = "The one Google account allowed to use the server."
  type        = string
  sensitive   = true
}

variable "enable_cloudsql" {
  description = "Create the Cloud SQL (PostgreSQL) store. Off by default: see cloudsql.tf and docs/cloud-sql-design.md."
  type        = bool
  default     = false
}

variable "store_backend" {
  description = "Which store the server uses: firestore (the default) or cloudsql. Switching to cloudsql needs enable_cloudsql and a migrated, imported database."
  type        = string
  default     = "firestore"
  validation {
    condition     = contains(["firestore", "cloudsql"], var.store_backend)
    error_message = "store_backend must be firestore or cloudsql."
  }
}

variable "cloudsql_tier" {
  description = "Cloud SQL machine tier. The smallest shared-core tier is enough for one user."
  type        = string
  default     = "db-f1-micro"
}

variable "cloudsql_database" {
  description = "Name of the database inside the Cloud SQL instance."
  type        = string
  default     = "options_book"
}

variable "cloudsql_budget_usd" {
  description = "Monthly budget amount (whole USD) once Cloud SQL is enabled, replacing the $1 guard. An alert threshold, not a cap. 20 covers the default instance (list prices of 2026-11-20: about $10 a month, up to about $17.50 if the public IPv4 address is billed at $0.01 an hour); re-check before changing the tier or disk."
  type        = string
  default     = "20"
}

variable "config_bucket" {
  description = "Name of a private Cloud Storage bucket to create for the real accounts.json and books/<book>/{book,rules}.json, which the server reads at startup (CONFIG_BUCKET). Empty (the default) keeps the compiled-in defaults, which in the public repo are synthetic. Keep the name out of the repo, as with the state bucket."
  type        = string
  default     = ""
}

variable "name_prefix" {
  description = "Prefix for the Artifact Registry repository id and the display names and titles of the service accounts, custom roles and budget. The default suits a new project. Changing it on a project that already exists renames display names in place, but the repository id cannot change: Terraform would delete and recreate the repository, and its images with it. A project created under another name keeps that name here."
  type        = string
  default     = "options-book"
}
