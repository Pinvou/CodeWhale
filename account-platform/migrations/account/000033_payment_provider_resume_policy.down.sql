DELETE FROM approval_command_policies WHERE command_type='PAYMENT_PROVIDER_RESUME' AND policy_version=1;
DELETE FROM role_permissions WHERE permission_code IN ('platform.billing.payment-controls.resume.request','platform.billing.payment-controls.resume.approve');
DELETE FROM permissions WHERE code IN ('platform.billing.payment-controls.resume.request','platform.billing.payment-controls.resume.approve');
