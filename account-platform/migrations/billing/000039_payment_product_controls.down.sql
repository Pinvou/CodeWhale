UPDATE recharge_products product
   SET enabled=FALSE
 WHERE EXISTS(
       SELECT 1
         FROM payment_product_controls control
        WHERE control.product_id=product.product_id
          AND NOT control.new_orders_enabled
          AND control.reason_code='AUTOMATIC_STOP_LOSS'
   );

DROP TABLE IF EXISTS payment_product_controls;
