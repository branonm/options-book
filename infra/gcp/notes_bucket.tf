# The owner's working notes (PLAN.md, BUGS.md, design docs), kept out of git. The
# server (NOTES_BUCKET) reads and writes notes/<name>.md here through notes_list,
# notes_read and notes_write; no Claude session has credentials of its own.
#
# A separate bucket on purpose: not the Terraform state bucket, which the
# server's service account must never touch, and not the config bucket, which it
# can only read and should stay that way.
#
# Versioning is on and there is no lifecycle rule, so every generation of every
# note is kept. The server has no delete path; objectUser (not objectCreator)
# is needed because replacing a note's current object takes storage.objects.delete,
# which versioning turns into "archive the old generation".
resource "google_storage_bucket" "notes" {
  count                       = var.notes_bucket == "" ? 0 : 1
  name                        = var.notes_bucket
  location                    = var.region
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"
  force_destroy               = false
  versioning {
    enabled = true
  }
  depends_on = [google_project_service.apis]
}

resource "google_storage_bucket_iam_member" "notes_writer" {
  count  = var.notes_bucket == "" ? 0 : 1
  bucket = google_storage_bucket.notes[0].name
  role   = "roles/storage.objectUser"
  member = "serviceAccount:${google_service_account.mcp.email}"
}
