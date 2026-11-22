export const entitlementHostID = n => `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`;
export function entitlementHostFacts(wallet = '9007199254740993') {
 const id = entitlementHostID;
 return { command_type:'ENTITLEMENT_COMPONENT_ADJUSTMENT',policy_version:2,snapshot_schema_version:2,target_resource_type:'ENTITLEMENT_PACKAGE',target_resource_id:id(5),
 normalized_command:{package_id:id(5),payment_order_id:id(6),reason_code:'PRODUCT_SPLIT_CORRECTION'},
 critical_resource_versions:{package_id:id(5),wallet_id:id(7),wallet_version:wallet,payment_order_id:id(6),payment_transaction_id:id(8),source_component_id:id(11),source_lot_id:id(12),target_component_id:null,target_lot_id:null,source_type:'BASE',target_type:'BONUS',amount_microcredits:'9007199254740993',source_evidence_digest:'a'.repeat(64),facts_digest:'b'.repeat(64),policy_version:2},
 financial_snapshot:{package_total_microcredits:'9223372036854775807',source_original_microcredits:'9223372036854775807',target_original_microcredits:0,effective_total_microcredits:'9223372036854775807'} };
}
