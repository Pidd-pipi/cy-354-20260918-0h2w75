package repository

import (
	"context"

	"gorm.io/gorm"
)

// activeOrderProductExpr is a stored generated column that records the
// product id of an unfinished order (pending/confirmed) and is NULL for
// completed/cancelled orders. Together with uk_trade_orders_active_product
// it enforces at the storage layer that one product can have at most one
// active order, even under concurrent requests that bypass service checks.
const (
	activeOrderProductColumn = "active_product_id"
	activeOrderProductExpr   = "(CASE WHEN `status` IN ('pending','confirmed') THEN `product_id` ELSE NULL END)"
	activeOrderUniqueIndex   = "uk_trade_orders_active_product"
)

// EnsureTradeOrderInvariants installs the single-active-order-per-product
// database invariant. It is idempotent and safe to run on databases created
// before the reservation loop existed: legacy duplicate active orders are
// reconciled (earliest kept, later ones cancelled) and product statuses are
// brought into line before the unique index is added.
func EnsureTradeOrderInvariants(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1) Repair any legacy rows that predate the invariant so adding the
		//    unique index cannot fail on duplicate active orders. DDL in
		//    MySQL commits implicitly, hence all DML reconciliation runs
		//    before any ALTER TABLE.
		if err := reconcileLegacyActiveOrders(tx); err != nil {
			return err
		}
		if err := reconcileLegacyProductStatuses(tx); err != nil {
			return err
		}

		// 2) Add the generated column if missing.
		var colN int64
		if err := tx.Raw(`
			SELECT COUNT(1) FROM information_schema.columns
			WHERE table_schema = DATABASE()
			  AND table_name = 'trade_orders'
			  AND column_name = ?`,
			activeOrderProductColumn,
		).Scan(&colN).Error; err != nil {
			return err
		}
		if colN == 0 {
			if err := tx.Exec(
				"ALTER TABLE trade_orders ADD COLUMN " + activeOrderProductColumn +
					" BIGINT UNSIGNED GENERATED ALWAYS AS " + activeOrderProductExpr + " STORED",
			).Error; err != nil {
				return err
			}
		}

		// 3) Add the unique index if missing (data is conflict-free now).
		var idxN int64
		if err := tx.Raw(`
			SELECT COUNT(1) FROM information_schema.statistics
			WHERE table_schema = DATABASE()
			  AND table_name = 'trade_orders'
			  AND index_name = ?`,
			activeOrderUniqueIndex,
		).Scan(&idxN).Error; err != nil {
			return err
		}
		if idxN == 0 {
			if err := tx.Exec(
				"ALTER TABLE trade_orders ADD UNIQUE INDEX " + activeOrderUniqueIndex +
					" (" + activeOrderProductColumn + ")",
			).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// reconcileLegacyActiveOrders cancels all but the earliest unfinished order
// of each product, the same outcome the reservation loop now enforces.
func reconcileLegacyActiveOrders(tx *gorm.DB) error {
	return tx.Exec(`
		UPDATE trade_orders t
		JOIN (
			SELECT product_id, MIN(id) AS keep_id
			FROM trade_orders
			WHERE status IN ('pending','confirmed')
			GROUP BY product_id
			HAVING COUNT(1) > 1
		) d ON t.product_id = d.product_id AND t.id <> d.keep_id
		SET t.status = 'cancelled'
		WHERE t.status IN ('pending','confirmed')`,
	).Error
}

// reconcileLegacyProductStatuses aligns product status with order state:
// products carrying an active order become reserved; reserved products
// without one go back on sale. Sold/removed products are never touched.
func reconcileLegacyProductStatuses(tx *gorm.DB) error {
	if err := tx.Exec(`
		UPDATE products p
		JOIN (
			SELECT product_id FROM trade_orders
			WHERE status IN ('pending','confirmed')
			GROUP BY product_id
		) a ON a.product_id = p.id
		SET p.status = 'reserved'
		WHERE p.status NOT IN ('sold','removed')`,
	).Error; err != nil {
		return err
	}
	return tx.Exec(`
		UPDATE products p
		LEFT JOIN (
			SELECT product_id FROM trade_orders
			WHERE status IN ('pending','confirmed')
			GROUP BY product_id
		) a ON a.product_id = p.id
		SET p.status = 'on_sale'
		WHERE p.status = 'reserved' AND a.product_id IS NULL`,
	).Error
}
