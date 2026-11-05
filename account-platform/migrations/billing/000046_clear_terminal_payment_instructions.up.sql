UPDATE payment_orders
SET payment_url = NULL,
    qr_data = NULL
WHERE (payment_url IS NOT NULL OR qr_data IS NOT NULL)
  AND (
    status NOT IN ('CREATED', 'PENDING')
    OR close_status <> 'OPEN'
  );
