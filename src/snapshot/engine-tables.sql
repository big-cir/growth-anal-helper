-- 엔진이 모든 스냅샷에 만드는 표.
CREATE TABLE snapshot_meta (
  snapshot_id TEXT,
  source_cutoff_at TEXT NOT NULL,
  collection_started_at TEXT NOT NULL,
  collection_finished_at TEXT NOT NULL,
  raw_hash TEXT,
  derived_hash TEXT,
  roles_hash TEXT,
  params_hash TEXT,
  spec_hash TEXT NOT NULL
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
