UPDATE approval_command_policies
   SET critical_resource_fields='["provider_id","provider_version","provider_control_version","product_control_versions","probe_order_id","policy_version"]'
 WHERE command_type='PAYMENT_PROVIDER_RESUME' AND policy_version=1;
DELETE FROM approval_command_policies WHERE command_type='PAYMENT_PRODUCT_RESUME' AND policy_version=1;
