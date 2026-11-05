SET ROLE account_service;

-- Preserve original 087 and close its nullable CHECK truth-table gap. Existing
-- invalid EVALUATE jobs block installation; no eligibility facts are rewritten.
ALTER TABLE wallet_provisioning_jobs
    ADD CONSTRAINT wallet_provisioning_registration_bonus_required_facts_check CHECK (
        registration_bonus_mode <> 'EVALUATE'
        OR (
            registration_operation_id IS NOT NULL
            AND registration_bonus_fingerprint_key_id IS NOT NULL
            AND registration_bonus_fingerprint IS NOT NULL
            AND length(registration_bonus_fingerprint_key_id) BETWEEN 1 AND 100
            AND octet_length(registration_bonus_fingerprint) = 32
        )
    );
