-- Tables the engine creates in every snapshot.
CREATE TABLE snapshot_meta (
  snapshot_id TEXT,
  source_cutoff_at TEXT NOT NULL,
  collection_started_at TEXT NOT NULL,
  collection_finished_at TEXT NOT NULL,
  raw_hash TEXT,
  derived_hash TEXT,
  roles_hash TEXT,
  params_hash TEXT,
  spec_hash TEXT NOT NULL,
  ga4_spec_hash TEXT NOT NULL DEFAULT ''
);
CREATE TABLE snapshot_params (key TEXT NOT NULL, value TEXT NOT NULL);
CREATE TABLE r_collect_log (
  table_name TEXT PRIMARY KEY,
  rows INTEGER NOT NULL,
  ms INTEGER NOT NULL,
  collected_at TEXT NOT NULL,
  dropped_after_cutoff INTEGER NOT NULL DEFAULT 0,
  nulled_after_cutoff INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE r_ga4_collect_log (
  report TEXT PRIMARY KEY,
  table_name TEXT NOT NULL,
  rows INTEGER NOT NULL,
  row_count INTEGER NOT NULL,
  dropped_small INTEGER NOT NULL,
  dropped_unobserved INTEGER NOT NULL,
  dropped_unmapped INTEGER NOT NULL,
  dropped_collision INTEGER NOT NULL,
  suppressed_cells INTEGER NOT NULL,
  subject_to_thresholding INTEGER NOT NULL,
  data_loss_from_other_row INTEGER NOT NULL,
  sampled INTEGER NOT NULL,
  sampling_summary TEXT NOT NULL,
  truncated TEXT NOT NULL,
  schema_restricted INTEGER NOT NULL,
  empty_reason TEXT NOT NULL,
  time_zone TEXT NOT NULL,
  data_through TEXT NOT NULL,
  calls INTEGER NOT NULL,
  quota_day_remaining INTEGER,
  quota_hour_remaining INTEGER,
  collected_at TEXT NOT NULL
);
