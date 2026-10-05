package billing

import "context"

// The plan migration retires the old personal offers. It must not remove the
// preceding organization's ordinary recharge contract. This exception is only
// for the four unchanged legacy offers through the development simulator.
func originalOrganizationProduct(product Product) bool {
	amounts := map[string]int64{"CNY_1": 100, "CNY_10": 1000, "CNY_100": 10000, "CNY_200": 20000}
	amount, ok := amounts[product.ProductID]
	return ok && product.PriceStatus == "LEGACY" && product.Currency == "CNY" && product.SKUCode == "" && product.ValidityDays == 0 && product.AmountFen == amount && product.DisplayPoints == amount && product.CreditMicrocredits == amount*10000
}

func (r *PostgresRepository) organizationProducts(ctx context.Context, currency, channel string) ([]Product, error) {
	rows, err := r.pool.Query(ctx, `SELECT product_id,plan_name,sku_code,amount_fen,credit_microcredits,display_points,base_points,bonus_points,validity_days,price_status,invoice_product_name,currency,channels,catalog_version FROM recharge_products WHERE product_id IN ('CNY_1','CNY_10','CNY_100','CNY_200') AND price_status='LEGACY' AND currency=$1 AND ($2='' OR $2=ANY(channels)) ORDER BY amount_fen`, currency, channel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	products := make([]Product, 0)
	for rows.Next() {
		var product Product
		if err := rows.Scan(&product.ProductID, &product.PlanName, &product.SKUCode, &product.AmountFen, &product.CreditMicrocredits, &product.DisplayPoints, &product.BasePoints, &product.BonusPoints, &product.ValidityDays, &product.PriceStatus, &product.InvoiceProductName, &product.Currency, &product.Channels, &product.CatalogVersion); err != nil {
			return nil, err
		}
		if originalOrganizationProduct(product) {
			products = append(products, product)
		}
	}
	return products, rows.Err()
}
