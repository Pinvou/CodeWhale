DO $guard$
BEGIN
 RAISE EXCEPTION USING ERRCODE='55000', MESSAGE='identity recovery generations and profile dispatch facts are forward-only';
END
$guard$;
