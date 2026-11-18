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
