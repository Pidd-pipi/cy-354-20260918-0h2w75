package repository

import (
	"context"
	"errors"

	"github.com/go-sql-driver/mysql"
	"github.com/lp/campus-market/internal/constants"
	"github.com/lp/campus-market/internal/model"
	"github.com/lp/campus-market/internal/util"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// mysqlDuplicateEntry is the ER_DUP_ENTRY code raised by MySQL/MariaDB when a
// unique constraint (e.g. one active order per product) is violated.
const mysqlDuplicateEntry = 1062

// IsDuplicateKeyErr reports whether err is a unique-constraint violation.
func IsDuplicateKeyErr(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == mysqlDuplicateEntry
}

// TradeOrderRepository persists trade order rows.
type TradeOrderRepository struct {
	db *gorm.DB
}

// NewTradeOrderRepository builds a TradeOrderRepository.
func NewTradeOrderRepository(db *gorm.DB) *TradeOrderRepository {
	return &TradeOrderRepository{db: db}
}

// Transaction runs fn inside a database transaction for cross-repository writes.
func (r *TradeOrderRepository) Transaction(ctx context.Context, fn func(txCtx context.Context) error) error {
	return Transaction(ctx, r.db, fn)
}

// Create inserts a new trade order.
func (r *TradeOrderRepository) Create(ctx context.Context, o *model.TradeOrder) error {
	return db(ctx, r.db).Create(o).Error
}

// FindByID returns a trade order by id.
func (r *TradeOrderRepository) FindByID(ctx context.Context, id uint) (*model.TradeOrder, error) {
	var o model.TradeOrder
	err := db(ctx, r.db).First(&o, id).Error
	if err != nil {
		return nil, normalizeError(err)
	}
	return &o, nil
}

// FindByIDForUpdate locks the order row (SELECT ... FOR UPDATE) inside a
// transaction. Taking the primary-key lock before any status UPDATE forces all
// concurrent state transitions (confirm/cancel) to serialize in the same lock
// order, avoiding deadlocks between the PRIMARY and status secondary index.
func (r *TradeOrderRepository) FindByIDForUpdate(ctx context.Context, id uint) (*model.TradeOrder, error) {
	var o model.TradeOrder
	err := db(ctx, r.db).Clauses(clause.Locking{Strength: "UPDATE"}).First(&o, id).Error
	if err != nil {
		return nil, normalizeError(err)
	}
	return &o, nil
}

// IsDeadlockErr reports whether err is an InnoDB deadlock/lock-wait rollback
// (Error 1213), which the caller may safely retry.
func IsDeadlockErr(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1213
}

// FindByProductAndBuyer returns an active order of a buyer for a product.
func (r *TradeOrderRepository) FindByProductAndBuyer(ctx context.Context, productID, buyerID uint) (*model.TradeOrder, error) {
	var o model.TradeOrder
	err := db(ctx, r.db).
		Where("product_id = ? AND buyer_id = ? AND status IN ?", productID, buyerID, constants.ActiveTradeStatuses).
		First(&o).Error
	if err != nil {
		return nil, normalizeError(err)
	}
	return &o, nil
}

// CountActiveByProduct counts the pending/confirmed orders bound to a product.
// Used inside the reservation transaction as a defense-in-depth check behind
// the row lock and the unique active-order index.
func (r *TradeOrderRepository) CountActiveByProduct(ctx context.Context, productID uint) (int64, error) {
	var n int64
	err := db(ctx, r.db).Model(&model.TradeOrder{}).
		Where("product_id = ? AND status IN ?", productID, constants.ActiveTradeStatuses).
		Count(&n).Error
	return n, err
}

// UpdateCancelledFrom moves an order from one of the given pre-states to
// cancelled. Returns util.ErrConflict when the order has already left those
// states (e.g. completed by a concurrent confirmation), so cancellation and
// confirmation cannot both succeed.
func (r *TradeOrderRepository) UpdateCancelledFrom(ctx context.Context, id uint, from []string) error {
	res := db(ctx, r.db).Model(&model.TradeOrder{}).
		Where("id = ? AND status IN ?", id, from).
		Update("status", constants.TradeStatusCancelled)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return util.ErrConflict
	}
	return nil
}

// ListByUser returns orders where the user is buyer or seller.
func (r *TradeOrderRepository) ListByUser(ctx context.Context, userID uint, page, pageSize int) ([]model.TradeOrder, int64, error) {
	q := db(ctx, r.db).Model(&model.TradeOrder{}).Where("buyer_id = ? OR seller_id = ?", userID, userID)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var items []model.TradeOrder
	err := q.Order("created_at DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&items).Error
	if err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// UpdateStatus sets the order status.
func (r *TradeOrderRepository) UpdateStatus(ctx context.Context, id uint, status string) error {
	res := db(ctx, r.db).Model(&model.TradeOrder{}).Where("id = ?", id).Update("status", status)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return util.ErrNotFound
	}
	return nil
}

// UpdateBuyerConfirmed sets the buyer confirmation timestamp and status.
func (r *TradeOrderRepository) UpdateBuyerConfirmed(ctx context.Context, id uint, ts interface{}) error {
	res := db(ctx, r.db).Model(&model.TradeOrder{}).Where("id = ? AND status = ?", id, constants.TradeStatusPending).
		Updates(map[string]interface{}{"buyer_confirmed_at": ts, "status": constants.TradeStatusConfirmed})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return util.ErrConflict
	}
	return nil
}

// UpdateSellerConfirmed sets the seller confirmation timestamp and completes the order.
func (r *TradeOrderRepository) UpdateSellerConfirmed(ctx context.Context, id uint, ts interface{}) error {
	res := db(ctx, r.db).Model(&model.TradeOrder{}).Where("id = ? AND status = ?", id, constants.TradeStatusConfirmed).
		Updates(map[string]interface{}{"seller_confirmed_at": ts, "completed_at": ts, "status": constants.TradeStatusCompleted})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return util.ErrConflict
	}
	return nil
}
