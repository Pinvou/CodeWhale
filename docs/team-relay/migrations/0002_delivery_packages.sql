-- Add an immutable delivery package envelope to each relay action.
-- Existing actions receive a role derived from their action kind so deployed
-- demo data remains readable without a destructive migration.

BEGIN;

ALTER TABLE relay_actions
    ADD COLUMN delivery jsonb NOT NULL DEFAULT '{
      "schema_version": 1,
      "artifact_role": "intermediate",
      "expected_next": "",
      "open_questions": "",
      "git_references": []
    }'::jsonb;

UPDATE relay_actions
   SET delivery = jsonb_set(
       delivery,
       '{artifact_role}',
       to_jsonb(CASE
           WHEN kind IN ('assign', 'request_changes') THEN 'input'
           WHEN kind IN ('final_deliver', 'accept') THEN 'final'
           WHEN kind = 'note' THEN 'evidence'
           ELSE 'intermediate'
       END::text)
   );

ALTER TABLE relay_actions
    ADD CONSTRAINT relay_actions_delivery_object_check
    CHECK (jsonb_typeof(delivery) = 'object');

COMMENT ON COLUMN relay_actions.delivery IS
    'Immutable delivery package metadata. Uploaded attachments and Git references are snapshots/evidence for this action; Git remains authoritative for repository content.';

INSERT INTO team_relay_schema_migrations (version) VALUES ('0002_delivery_packages');

COMMIT;
