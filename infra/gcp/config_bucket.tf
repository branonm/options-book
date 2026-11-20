# The real account and rule configuration, kept out of the image and the repo.
# The server (CONFIG_BUCKET) reads config/accounts.json and
# books/<book>/{book,rules}.json from here at startup and refuses to start if
# they are unreadable. It can read, never write: a change is an upload by the
# owner, and versioning keeps every earlier generation. The generation numbers
# read are logged at startup.
resource "google_storage_bucket" "config" {
  count                       = var.config_bucket == "" ? 0 : 1
  name                        = var.config_bucket
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = false
  versioning {
    enabled = true
  }
  depends_on = [google_project_service.apis]
}

resource "google_storage_bucket_iam_member" "config_reader" {
  count  = var.config_bucket == "" ? 0 : 1
  bucket = google_storage_bucket.config[0].name
  role   = "roles/storage.objectViewer"
  member = "serviceAccount:${google_service_account.mcp.email}"
}
