ALTER TABLE ops_error_logs ADD COLUMN IF NOT EXISTS triage_class VARCHAR(64) NOT NULL DEFAULT 'unclassified';

CREATE INDEX IF NOT EXISTS idx_ops_error_logs_triage_class ON ops_error_logs(triage_class);

-- Backfill historical rows with the same rules as ClassifyErrorTriage.
UPDATE ops_error_logs
SET triage_class = CASE
  WHEN COALESCE(error_message, '') ILIKE '%truncat%' OR COALESCE(upstream_error_message, '') ILIKE '%truncat%' THEN 'stream_truncated'
  WHEN COALESCE(error_message, '') ILIKE '%timeout%' OR COALESCE(upstream_error_message, '') ILIKE '%timeout%'
    OR COALESCE(error_message, '') ILIKE '%deadline%' OR COALESCE(upstream_error_message, '') ILIKE '%deadline%' THEN 'upstream_timeout'
  WHEN LOWER(COALESCE(error_type, '')) = 'model_not_found'
    OR COALESCE(error_message, '') ILIKE '%not available for this group%'
    OR COALESCE(error_message, '') ILIKE '%not supported by any%' THEN 'routing_not_found'
  WHEN LOWER(COALESCE(error_phase, '')) = 'routing' AND status_code = 503 THEN 'routing_breaker'
  WHEN upstream_status_code IN (401, 403) THEN 'upstream_auth'
  WHEN upstream_status_code = 400 THEN 'upstream_validation'
  WHEN upstream_status_code = 404 THEN 'upstream_not_found'
  WHEN upstream_status_code >= 500 THEN 'upstream_server'
  WHEN LOWER(COALESCE(error_phase, '')) = 'upstream' AND status_code IN (401, 403) THEN 'upstream_auth'
  WHEN LOWER(COALESCE(error_phase, '')) = 'upstream' AND status_code = 400 THEN 'upstream_validation'
  WHEN LOWER(COALESCE(error_phase, '')) = 'upstream' AND status_code = 404 THEN 'upstream_not_found'
  WHEN LOWER(COALESCE(error_phase, '')) = 'upstream' AND status_code >= 500 THEN 'upstream_server'
  ELSE 'unclassified'
END
WHERE triage_class = 'unclassified';
