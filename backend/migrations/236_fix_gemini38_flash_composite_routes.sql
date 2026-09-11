-- Public Gemini 3.8 Flash uses the -high wire ID. Bare gemini-3.8-flash 404s
-- upstream ("Requested entity was not found"). -high/-low/-medium/-tiered share
-- the same quota; this is the requested default alias, not a separate bucket.

UPDATE composite_model_routes
SET upstream_model = 'gemini-3.8-flash-high',
    notes = 'Public Gemini 3.8 Flash -> gemini-3.8-flash-high. Bare gemini-3.8-flash is not a wire ID.',
    updated_at = NOW()
WHERE deleted_at IS NULL
  AND enabled
  AND target_platform = 'antigravity'
  AND (
    public_model = 'gemini-3.8-flash'
    OR public_model = 'gemini-3.8-flash-tiered'
    OR public_model = 'sub2api/gemini-3.8-flash'
    OR public_model LIKE 'sub2/gem38%'
  );
