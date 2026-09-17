package repository

import (
	"context"

	"github.com/lp/campus-market/internal/constants"
	"github.com/lp/campus-market/internal/model"
	"github.com/lp/campus-market/internal/util"
	"gorm.io/gorm"
)

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
	return normalizeError(db(ctx, r.db).Create(o).Error)
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

// CancelIfUnfinished atomically cancels an order that is still pending or
// confirmed. It returns util.ErrConflict when the order is already
// completed or cancelled, so duplicate and late cancels fail explicitly.
func (r *TradeOrderRepository) CancelIfUnfinished(ctx context.Context, id uint) error {
	res := db(ctx, r.db).Model(&model.TradeOrder{}).
		Where("id = ? AND status IN ?", id, []string{constants.TradeStatusPending, constants.TradeStatusConfirmed}).
		Update("status", constants.TradeStatusCancelled)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return util.ErrConflict
	}
	return nil
}

// UpdateBuyerConfirmed sets the buyer confirmation timestamp and moves the
// order from pending to confirmed with a compare-and-set guard.
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

// UpdateSellerConfirmed sets the seller confirmation timestamp and completes
// the order with a compare-and-set guard, so a duplicate seller confirmation
// (or one before the buyer has confirmed) cannot rewrite a finished order.
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
