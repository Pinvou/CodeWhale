ALTER TABLE pricing_operation_commands
 DROP CONSTRAINT pricing_operation_commands_operation_check,
 ADD CONSTRAINT pricing_operation_commands_operation_check
 CHECK (operation IN ('UNIT_CREATE','PRICE_DRAFT_CREATE','PRICE_DRAFT_UPDATE','UNIT_DEACTIVATE'));
