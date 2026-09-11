-- Backfill persisted Antigravity model_mapping with Gemini 3.8 Flash IDs.
--
-- Accounts without a stored mapping already use DefaultAntigravityModelMapping.
-- This only fills missing keys on existing mapping objects so 3.8 Flash is not
-- dropped when an older whitelist was saved without the new models.

UPDATE accounts
SET credentials = jsonb_set(
    jsonb_set(
        jsonb_set(
            jsonb_set(
                jsonb_set(
                    credentials,
                    '{model_mapping,gemini-3.8-flash}',
                    '"gemini-3.8-flash"'::jsonb,
                    true
                ),
                '{model_mapping,gemini-3.8-flash-high}',
                '"gemini-3.8-flash-high"'::jsonb,
                true
            ),
            '{model_mapping,gemini-3.8-flash-low}',
            '"gemini-3.8-flash-low"'::jsonb,
            true
        ),
        '{model_mapping,gemini-3.8-flash-medium}',
        '"gemini-3.8-flash-medium"'::jsonb,
        true
    ),
    '{model_mapping,gemini-3.8-flash-tiered}',
    '"gemini-3.8-flash-tiered"'::jsonb,
    true
)
WHERE platform = 'antigravity'
  AND deleted_at IS NULL
  AND jsonb_typeof(credentials->'model_mapping') = 'object'
  AND (
    credentials->'model_mapping'->>'gemini-3.8-flash' IS NULL
    OR credentials->'model_mapping'->>'gemini-3.8-flash-high' IS NULL
    OR credentials->'model_mapping'->>'gemini-3.8-flash-low' IS NULL
    OR credentials->'model_mapping'->>'gemini-3.8-flash-medium' IS NULL
    OR credentials->'model_mapping'->>'gemini-3.8-flash-tiered' IS NULL
  );
