CREATE TABLE IF NOT EXISTS jobs (
  id                TEXT PRIMARY KEY,
  surface           TEXT NOT NULL,
  model_id          TEXT NOT NULL DEFAULT '',
  api_version       TEXT NOT NULL DEFAULT '',
  status            TEXT NOT NULL,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL,
  expires_at        INTEGER NOT NULL,
  request_query     TEXT NOT NULL DEFAULT '',
  content_type      TEXT NOT NULL DEFAULT '',
  input_path        TEXT NOT NULL DEFAULT '',
  input_bytes       INTEGER NOT NULL DEFAULT 0,
  result_path       TEXT NOT NULL DEFAULT '',
  result_bytes      INTEGER NOT NULL DEFAULT 0,
  error_json        TEXT NOT NULL DEFAULT '',
  attempts          INTEGER NOT NULL DEFAULT 0,
  lease_owner       TEXT NOT NULL DEFAULT '',
  lease_until       INTEGER NOT NULL DEFAULT 0,
  upstream_mode     TEXT NOT NULL DEFAULT '',
  upstream_op_url   TEXT NOT NULL DEFAULT '',
  upstream_status   INTEGER NOT NULL DEFAULT 0,
  upstream_ms       INTEGER NOT NULL DEFAULT 0,
  client_request_id TEXT NOT NULL DEFAULT '',
  has_pdf           INTEGER NOT NULL DEFAULT 0,
  figures           TEXT NOT NULL DEFAULT '',
  prefix            TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS jobs_status  ON jobs(status, created_at);
CREATE INDEX IF NOT EXISTS jobs_expiry  ON jobs(expires_at);
CREATE INDEX IF NOT EXISTS jobs_surface ON jobs(surface, status);
