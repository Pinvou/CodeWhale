-- Policy versions may be referenced by Token's independently owned command
-- history. Cross-database absence cannot be proven here: preserve v3 as well
-- as v1/v2. Rolling back an application is not permission to erase evidence.
SELECT 1;
