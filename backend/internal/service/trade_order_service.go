package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/lp/campus-market/internal/constants"
	"github.com/lp/campus-market/internal/dto"
	"github.com/lp/campus-market/internal/model"
	"github.com/lp/campus-market/internal/repository"
	"github.com/lp/campus-market/internal/util"
)

// TradeOrderService manages purchase intents, confirmations and completion.
type TradeOrderService struct {
	orders   *repository.TradeOrderRepository
	products *repository.ProductRepository
	logger   *slog.Logger
}

// NewTradeOrderService wires the trade order service dependencies.
func NewTradeOrderService(orders *repository.TradeOrderRepository, products *repository.ProductRepository, logger *slog.Logger) *TradeOrderService {
	return &TradeOrderService{orders: orders, products: products, logger: logger}
}

// Create creates the single pending order for an on-sale product and flips
// the product to reserved. Product rows are locked first inside one
// transaction so concurrent purchases of the same item serialize: exactly
// one wins, the rest fail with a conflict and no duplicate order is kept.
// A generated-column unique index in the database is the final guard.
func (s *TradeOrderService) Create(ctx context.Context, buyer *model.User, req *dto.CreateTradeOrderRequest) (*model.TradeOrder, error) {
	var created *model.TradeOrder
	err := s.orders.Transaction(ctx, func(txCtx context.Context) error {
		product, err := s.products.LockByID(txCtx, req.ProductID)
		if err != nil {
			if errors.Is(err, util.ErrNotFound) {
				return util.WrapAppError(fmt.Errorf("trade_order[buyer=%d] product lookup: %w", buyer.ID, err), 404, constants.CodeNotFound, constants.MsgNotFound)
			}
			return util.WrapAppError(fmt.Errorf("trade_order[buyer=%d] product lock: %w", buyer.ID, err), 500, constants.CodeInternalError, constants.MsgInternalError)
		}
		if product.SellerID == buyer.ID {
			return util.NewAppError(400, constants.CodeBadRequest, "不能购买自己的商品", nil)
		}
		if product.Status == constants.ProductStatusReserved {
			return util.NewAppError(409, constants.CodeConflict, constants.MsgProductReserved, nil)
		}
		if product.Status == constants.ProductStatusSold {
			return util.NewAppError(409, constants.CodeConflict, constants.MsgProductSold, nil)
		}
		if product.Status != constants.ProductStatusOnSale {
			return util.NewAppError(409, constants.CodeConflict, constants.MsgProductNotOnSale, nil)
		}
		order := &model.TradeOrder{
			ProductID: req.ProductID, BuyerID: buyer.ID, SellerID: product.SellerID,
			Status: constants.TradeStatusPending,
		}
		if err := s.orders.Create(txCtx, order); err != nil {
			if errors.Is(err, util.ErrConflict) {
				return util.NewAppError(409, constants.CodeConflict, constants.MsgProductReserved, nil)
			}
			return util.WrapAppError(fmt.Errorf("trade_order[buyer=%d] create: %w", buyer.ID, err), 500, constants.CodeInternalError, constants.MsgInternalError)
		}
		if err := s.products.UpdateStatusFromTo(txCtx, req.ProductID, constants.ProductStatusOnSale, constants.ProductStatusReserved); err != nil {
			if errors.Is(err, util.ErrConflict) {
				return util.NewAppError(409, constants.CodeConflict, constants.MsgProductReserved, nil)
			}
			return util.WrapAppError(fmt.Errorf("product[id=%d] reserve: %w", req.ProductID, err), 500, constants.CodeInternalError, constants.MsgInternalError)
		}
		created = order
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.logger.Info(fmt.Sprintf(constants.LogTradeOrderCreateSuccess, created.ID, req.ProductID))
	s.logger.Info(fmt.Sprintf(constants.LogProductReservedSuccess, req.ProductID))
	return created, nil
}

// ListMy returns the orders where the user participates, with product
// snapshots attached so refreshed lists stay consistent with product state.
func (s *TradeOrderService) ListMy(ctx context.Context, userID uint, q *dto.PageQuery) (*dto.PageResult, error) {
	q.Normalize()
	items, total, err := s.orders.ListByUser(ctx, userID, q.Page, q.PageSize)
	if err != nil {
		return nil, util.WrapAppError(fmt.Errorf("trade_order[user=%d] list: %w", userID, err), 500, constants.CodeInternalError, constants.MsgInternalError)
	}
	if err := s.attachProducts(ctx, items); err != nil {
		return nil, err
	}
	return &dto.PageResult{Items: items, Total: total, Page: q.Page, PageSize: q.PageSize}, nil
}

// attachProducts fills the non-persisted Product field of each order with a
// single batched lookup.
func (s *TradeOrderService) attachProducts(ctx context.Context, orders []model.TradeOrder) error {
	if len(orders) == 0 {
		return nil
	}
	ids := make([]uint, 0, len(orders))
	seen := make(map[uint]struct{}, len(orders))
	for i := range orders {
		if _, ok := seen[orders[i].ProductID]; ok {
			continue
		}
		seen[orders[i].ProductID] = struct{}{}
		ids = append(ids, orders[i].ProductID)
	}
	products, err := s.products.FindByIDs(ctx, ids)
	if err != nil {
		return util.WrapAppError(fmt.Errorf("trade_order attach products: %w", err), 500, constants.CodeInternalError, constants.MsgInternalError)
	}
	byID := make(map[uint]*model.Product, len(products))
	for i := range products {
		p := products[i]
		byID[p.ID] = &p
	}
	for i := range orders {
		orders[i].Product = byID[orders[i].ProductID]
	}
	return nil
}

// BuyerConfirm marks the order confirmed by the buyer (buyer confirms
// receipt). The compare-and-set update rejects duplicate or out-of-order
// confirmations.
func (s *TradeOrderService) BuyerConfirm(ctx context.Context, userID, orderID uint) (*model.TradeOrder, error) {
	order, err := s.orders.FindByID(ctx, orderID)
	if err != nil {
		return nil, util.WrapAppError(fmt.Errorf("trade_order[id=%d] buyer confirm find: %w", orderID, err), 404, constants.CodeNotFound, constants.MsgNotFound)
	}
	if order.BuyerID != userID {
		return nil, util.NewAppError(403, constants.CodeForbidden, constants.MsgNotParticipant, nil)
	}
	now := time.Now()
	if err := s.orders.UpdateBuyerConfirmed(ctx, orderID, now); err != nil {
		if errors.Is(err, util.ErrConflict) {
			return nil, util.NewAppError(409, constants.CodeConflict, constants.MsgTradeStatusInvalid, nil)
		}
		return nil, util.WrapAppError(fmt.Errorf("trade_order[id=%d] buyer confirm: %w", orderID, err), 500, constants.CodeInternalError, constants.MsgInternalError)
	}
	s.logger.Info(fmt.Sprintf(constants.LogTradeOrderBuyerConfirmSuccess, orderID))
	order.Status = constants.TradeStatusConfirmed
	order.BuyerConfirmedAt = &now
	return order, nil
}

// SellerConfirm completes the order after the buyer confirmed receipt and
// marks the product sold. Both writes share one transaction; the
// compare-and-set guards reject seller confirmation before buyer receipt and
// any repeated confirmation.
func (s *TradeOrderService) SellerConfirm(ctx context.Context, userID, orderID uint) (*model.TradeOrder, error) {
	order, err := s.orders.FindByID(ctx, orderID)
	if err != nil {
		return nil, util.WrapAppError(fmt.Errorf("trade_order[id=%d] seller confirm find: %w", orderID, err), 404, constants.CodeNotFound, constants.MsgNotFound)
	}
	if order.SellerID != userID {
		return nil, util.NewAppError(403, constants.CodeForbidden, constants.MsgNotParticipant, nil)
	}
	now := time.Now()
	if err := s.orders.Transaction(ctx, func(txCtx context.Context) error {
		if _, err := s.products.LockByID(txCtx, order.ProductID); err != nil {
			if errors.Is(err, util.ErrNotFound) {
				return util.WrapAppError(fmt.Errorf("product[id=%d] lock: %w", order.ProductID, err), 404, constants.CodeNotFound, constants.MsgNotFound)
			}
			return util.WrapAppError(fmt.Errorf("product[id=%d] lock: %w", order.ProductID, err), 500, constants.CodeInternalError, constants.MsgInternalError)
		}
		if err := s.orders.UpdateSellerConfirmed(txCtx, orderID, now); err != nil {
			if errors.Is(err, util.ErrConflict) {
				return util.NewAppError(409, constants.CodeConflict, constants.MsgTradeStatusInvalid, nil)
			}
			return util.WrapAppError(fmt.Errorf("trade_order[id=%d] seller confirm: %w", orderID, err), 500, constants.CodeInternalError, constants.MsgInternalError)
		}
		if err := s.products.UpdateStatusFromTo(txCtx, order.ProductID, constants.ProductStatusReserved, constants.ProductStatusSold); err != nil {
			if errors.Is(err, util.ErrConflict) {
				return util.NewAppError(409, constants.CodeConflict, constants.MsgTradeStatusInvalid, nil)
			}
			return util.WrapAppError(fmt.Errorf("product[id=%d] sold: %w", order.ProductID, err), 500, constants.CodeInternalError, constants.MsgInternalError)
		}
		return nil
	}); err != nil {
		// 409 是正常的状态冲突（顺序颠倒/重复确认），不写错误日志
		var appErr *util.AppError
		if errors.As(err, &appErr) && appErr.Status == 409 {
			return nil, err
		}
		s.logger.Error(fmt.Sprintf(constants.LogTradeOrderCompleteFailed, orderID, err))
		return nil, err
	}
	s.logger.Info(fmt.Sprintf(constants.LogTradeOrderCompleteSuccess, orderID, order.ProductID))
	s.logger.Info(fmt.Sprintf(constants.LogProductSoldSuccess, order.ProductID))
	order.Status = constants.TradeStatusCompleted
	order.SellerConfirmedAt = &now
	order.CompletedAt = &now
	return order, nil
}

// Cancel cancels an unfinished order by either the buyer or the seller and
// releases the product back to on_sale in the same transaction. Orders that
// are already completed or cancelled fail explicitly and leave product
// status untouched.
func (s *TradeOrderService) Cancel(ctx context.Context, userID, orderID uint) (*model.TradeOrder, error) {
	order, err := s.orders.FindByID(ctx, orderID)
	if err != nil {
		return nil, util.WrapAppError(fmt.Errorf("trade_order[id=%d] cancel find: %w", orderID, err), 404, constants.CodeNotFound, constants.MsgNotFound)
	}
	if order.BuyerID != userID && order.SellerID != userID {
		return nil, util.NewAppError(403, constants.CodeForbidden, constants.MsgNotParticipant, nil)
	}
	if err := s.orders.Transaction(ctx, func(txCtx context.Context) error {
		if _, err := s.products.LockByID(txCtx, order.ProductID); err != nil {
			if errors.Is(err, util.ErrNotFound) {
				return util.WrapAppError(fmt.Errorf("product[id=%d] lock: %w", order.ProductID, err), 404, constants.CodeNotFound, constants.MsgNotFound)
			}
			return util.WrapAppError(fmt.Errorf("product[id=%d] lock: %w", order.ProductID, err), 500, constants.CodeInternalError, constants.MsgInternalError)
		}
		if err := s.orders.CancelIfUnfinished(txCtx, orderID); err != nil {
			if errors.Is(err, util.ErrConflict) {
				return util.NewAppError(409, constants.CodeConflict, constants.MsgTradeStatusInvalid, nil)
			}
			return util.WrapAppError(fmt.Errorf("trade_order[id=%d] cancel: %w", orderID, err), 500, constants.CodeInternalError, constants.MsgInternalError)
		}
		if err := s.products.UpdateStatusFromTo(txCtx, order.ProductID, constants.ProductStatusReserved, constants.ProductStatusOnSale); err != nil {
			if errors.Is(err, util.ErrConflict) {
				return util.NewAppError(409, constants.CodeConflict, constants.MsgTradeStatusInvalid, nil)
			}
			return util.WrapAppError(fmt.Errorf("product[id=%d] release: %w", order.ProductID, err), 500, constants.CodeInternalError, constants.MsgInternalError)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	s.logger.Info(fmt.Sprintf(constants.LogTradeOrderCancelSuccess, orderID))
	s.logger.Info(fmt.Sprintf(constants.LogProductReleasedSuccess, order.ProductID))
	order.Status = constants.TradeStatusCancelled
	return order, nil
}
