# Negative adjustment and allocated member quota

An approved negative adjustment consumes only eligible unallocated component availability. Exhausting that availability cannot retire a component or package while reserved Hold, active member-grant or suspended member-grant capacity remains. The existing component REVOKED and package CONSUMED outcomes require all four live buckets to be empty; partial debits keep their existing status behavior.

This preserves allocated capacity and its original source, expiry and membership fences. A suspended member still cannot reserve, and an existing financial freeze is not lifted; only the existing authoritative lifecycle/financial recovery paths can restore eligibility. Adjustment request/independent approval, expiry order, BONUS/BASE ordering, ledger entries and original execution replay remain unchanged.

No historical component/package status is rewritten or guessed from present balances. The required real-PostgreSQL regression follows payment merging, allocation, independent approved debit, reservation/release and ordinary final settlement; it checks conservation, suspension/freeze rejection and failed/replayed commands without changing balances twice. Controlled provider fixtures do not represent live payment, Account approval or model-dispatch E2E.
