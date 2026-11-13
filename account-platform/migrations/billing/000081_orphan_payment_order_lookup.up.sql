-- Preserve complete order evidence and include unproven NULL payment dates.
-- This is a read access path, not authority to reconstruct a payment or credit.
-- Fresh 065/077 and hardened upgrades revoke runtime CREATE. The deployment
-- session already holds the separate owner role; lend no runtime privilege.
SET LOCAL ROLE billing_schema_owner;
CREATE INDEX payment_orders_orphan_lookup_idx
    ON public.payment_orders(provider_id, provider_version, channel, paid_at)
    WHERE status = 'PAID' AND fulfillment_status = 'CREDITED';
SET LOCAL ROLE billing_service;
