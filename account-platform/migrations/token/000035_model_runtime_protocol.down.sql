-- Never erase an explicitly published protocol fact or a configured draft.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM model_capability_versions WHERE runtime_protocol <> '')
       OR EXISTS (SELECT 1 FROM model_catalog_drafts WHERE runtime_protocol <> '') THEN
        RAISE EXCEPTION 'runtime protocol facts exist; rollback is forward-only';
    END IF;
END $$;

ALTER TABLE model_catalog_drafts DROP COLUMN runtime_protocol;
ALTER TABLE model_capability_versions DROP COLUMN runtime_protocol;
