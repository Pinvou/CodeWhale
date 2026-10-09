-- Policy versions may already be frozen by Token command results in another
-- database. Retain v3 and all historical versions when reverting this marker.
SELECT 1;
