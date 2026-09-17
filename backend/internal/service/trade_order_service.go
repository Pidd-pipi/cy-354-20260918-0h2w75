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

// Create creates the single pending order for an on-sale product and flips the
// product to "reserved" in one transaction. The product row is locked with
// SELECT ... FOR UPDATE, so concurrent purchase attempts serialize: exactly
// one order is created and every other attempt fails with a 409 without
// leaving a duplicate order.
func (s *TradeOrderService) Create(ctx context.Context, buyer *model.User, req *dto.CreateTradeOrderRequest) (*model.TradeOrder, error) {
	product, err := s.products.FindByID(ctx, req.ProductID)
	if err != nil {
		return nil, util.WrapAppError(fmt.Errorf("trade_order[buyer=%d] product lookup: %w", buyer.ID, err), 404, constants.CodeNotFound, constants.MsgNotFound)
	}
	if product.SellerID == buyer.ID {
		return nil, util.NewAppError(400, constants.CodeBadRequest, "不能购买自己的商品", nil)
	}

	order := &model.TradeOrder{
		ProductID: req.ProductID, BuyerID: buyer.ID, SellerID: product.SellerID,
		Status: constants.TradeStatusPending,
	}
	err = s.orders.Transaction(ctx, func(txCtx context.Context) error {
		// Lock the product row for the whole transaction: concurrent buyers
		// queue here and then observe the reserved status / active order.
		locked, err := s.products.FindByIDForUpdate(txCtx, req.ProductID)
		if err != nil {
			return util.WrapAppError(fmt.Errorf("trade_order[buyer=%d] lock product: %w", buyer.ID, err), 404, constants.CodeNotFound, constants.MsgNotFound)
		}
		if locked.SellerID == buyer.ID {
			return util.NewAppError(400, constants.CodeBadRequest, "不能购买自己的商品", nil)
		}
		if locked.Status != constants.ProductStatusOnSale {
			return s.productNotOnSaleError(req.ProductID, buyer.ID, locked.Status)
		}
		if existing, err := s.orders.FindByProductAndBuyer(txCtx, req.ProductID, buyer.ID); err == nil && existing != nil {
			return util.NewAppError(409, constants.CodeConflict, constants.MsgOwnOrderConflict, nil)
		} else if err != nil && !errors.Is(err, util.ErrNotFound) {
			return fmt.Errorf("trade_order[buyer=%d] duplicate lookup: %w", buyer.ID, err)
		}
		active, err := s.orders.CountActiveByProduct(txCtx, req.ProductID)
		if err != nil {
			return fmt.Errorf("trade_order[product=%d] active count: %w", req.ProductID, err)
		}
		if active > 0 {
			return util.NewAppError(409, constants.CodeConflict, constants.MsgProductReserved, nil)
		}
		if err := s.orders.Create(txCtx, order); err != nil {
			if repository.IsDuplicateKeyErr(err) {
				return util.NewAppError(409, constants.CodeConflict, constants.MsgProductReserved, nil)
			}
			return fmt.Errorf("trade_order[buyer=%d] create: %w", buyer.ID, err)
		}
		// Compare-and-set: only an on_sale product reaches reserved here.
		if err := s.products.UpdateStatusIfFrom(txCtx, req.ProductID,
			[]string{constants.ProductStatusOnSale}, constants.ProductStatusReserved); err != nil {
			return fmt.Errorf("trade_order[product=%d] reserve: %w", req.ProductID, err)
		}
		return nil
	})
	if err != nil {
		var appErr *util.AppError
		if errors.As(err, &appErr) {
			s.logger.Warn(fmt.Sprintf(constants.LogTradeOrderCreateConflict, req.ProductID, buyer.ID, appErr.Message))
			return nil, appErr
		}
		if errors.Is(err, util.ErrConflict) || repository.IsDuplicateKeyErr(err) {
			s.logger.Warn(fmt.Sprintf(constants.LogTradeOrderCreateConflict, req.ProductID, buyer.ID, err))
			return nil, util.NewAppError(409, constants.CodeConflict, constants.MsgProductReserved, nil)
		}
		return nil, util.WrapAppError(fmt.Errorf("trade_order[buyer=%d] create tx: %w", buyer.ID, err), 500, constants.CodeInternalError, constants.MsgInternalError)
	}
	s.logger.Info(fmt.Sprintf(constants.LogProductReservedSuccess, order.ProductID, order.ID))
	s.logger.Info(fmt.Sprintf(constants.LogTradeOrderCreateSuccess, order.ID, req.ProductID))
	return order, nil
}

// productNotOnSaleError builds the 409 response for a product that left the
// on-sale state, mentioning both the product field and the buyer role as the
// error-message convention requires.
func (s *TradeOrderService) productNotOnSaleError(productID, buyerID uint, status string) error {
	msg := constants.MsgProductNotOnSale
	if status == constants.ProductStatusReserved {
		msg = constants.MsgProductReserved
	}
	if status == constants.ProductStatusSold {
		msg = constants.MsgProductSold
	}
	return util.NewAppError(409, constants.CodeConflict,
		fmt.Sprintf("Product[id=%d].status=%s 不允许 buyer[%d] 下单：%s", productID, status, buyerID, msg), nil)
}

// ListMy returns the orders where the user participates.
func (s *TradeOrderService) ListMy(ctx context.Context, userID uint, q *dto.PageQuery) (*dto.PageResult, error) {
	q.Normalize()
	items, total, err := s.orders.ListByUser(ctx, userID, q.Page, q.PageSize)
	if err != nil {
		return nil, util.WrapAppError(fmt.Errorf("trade_order[user=%d] list: %w", userID, err), 500, constants.CodeInternalError, constants.MsgInternalError)
	}
	return &dto.PageResult{Items: items, Total: total, Page: q.Page, PageSize: q.PageSize}, nil
}

// maxStateMachineRetries bounds deadlock retries for order state transitions.
const maxStateMachineRetries = 3

// withDeadlockRetry retries a compare-and-set operation when InnoDB reports a
// deadlock (Error 1213). Every attempt re-reads its rows, so a lost race simply
// surfaces as a 409 on the retry instead of a double-write.
func withDeadlockRetry(fn func() error) error {
	var err error
	for attempt := 0; attempt < maxStateMachineRetries; attempt++ {
		err = fn()
		if !repository.IsDeadlockErr(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 10 * time.Millisecond)
	}
	return err
}

// isConflictError recognizes every shape a lost compare-and-set race takes:
// the repository sentinel, a duplicate key violation, or a wrapped 409 AppError
// produced after re-reading a row inside a retried transaction.
func isConflictError(err error) bool {
	if errors.Is(err, util.ErrConflict) || repository.IsDuplicateKeyErr(err) {
		return true
	}
	var appErr *util.AppError
	return errors.As(err, &appErr) && appErr.Status == 409
}

func conflictAppError(err error) error {
	if appErr := new(util.AppError); errors.As(err, &appErr) && appErr.Status == 409 {
		return appErr
	}
	return util.NewAppError(409, constants.CodeConflict, constants.MsgTradeStatusInvalid, nil)
}

// preserveAppError returns the AppError already carried by err (e.g. 403/404
// produced while validating the attempt) so the outer wrapper does not mask it
// as a generic 500; nil means err is an unexpected infrastructure error.
func preserveAppError(err error) *util.AppError {
	var appErr *util.AppError
	if errors.As(err, &appErr) {
		return appErr
	}
	return nil
}

// BuyerConfirm marks the order confirmed by the buyer (goods received). The
// product stays "reserved" until the seller confirms payment.
func (s *TradeOrderService) BuyerConfirm(ctx context.Context, userID, orderID uint) (*model.TradeOrder, error) {
	var result *model.TradeOrder
	err := withDeadlockRetry(func() error {
		order, err := s.orders.FindByID(ctx, orderID)
		if err != nil {
			return util.WrapAppError(fmt.Errorf("trade_order[id=%d] buyer confirm find: %w", orderID, err), 404, constants.CodeNotFound, constants.MsgNotFound)
		}
		if order.BuyerID != userID {
			return util.NewAppError(403, constants.CodeForbidden, constants.MsgNotParticipant, nil)
		}
		if order.Status != constants.TradeStatusPending {
			return util.NewAppError(409, constants.CodeConflict, constants.MsgTradeStatusInvalid, nil)
		}
		now := time.Now()
		if err := s.orders.UpdateBuyerConfirmed(ctx, orderID, now); err != nil {
			if errors.Is(err, util.ErrConflict) {
				return util.NewAppError(409, constants.CodeConflict, constants.MsgTradeStatusInvalid, nil)
			}
			return util.WrapAppError(fmt.Errorf("trade_order[id=%d] buyer confirm: %w", orderID, err), 500, constants.CodeInternalError, constants.MsgInternalError)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result, err = s.orders.FindByID(ctx, orderID)
	if err != nil {
		return nil, util.WrapAppError(fmt.Errorf("trade_order[id=%d] buyer confirm reload: %w", orderID, err), 500, constants.CodeInternalError, constants.MsgInternalError)
	}
	s.logger.Info(fmt.Sprintf(constants.LogTradeOrderBuyerConfirmSuccess, orderID))
	return result, nil
}

// SellerConfirm completes the order after the seller confirms payment and
// marks the product sold. The order PK row is locked first and every update is
// a compare-and-set, so duplicate confirmations cannot complete an order twice
// and concurrent cancel/confirm runs never deadlock or double-fire.
func (s *TradeOrderService) SellerConfirm(ctx context.Context, userID, orderID uint) (*model.TradeOrder, error) {
	err := withDeadlockRetry(func() error {
		order, err := s.orders.FindByID(ctx, orderID)
		if err != nil {
			return util.WrapAppError(fmt.Errorf("trade_order[id=%d] seller confirm find: %w", orderID, err), 404, constants.CodeNotFound, constants.MsgNotFound)
		}
		if order.SellerID != userID {
			return util.NewAppError(403, constants.CodeForbidden, constants.MsgNotParticipant, nil)
		}
		if order.Status != constants.TradeStatusConfirmed {
			return util.NewAppError(409, constants.CodeConflict, constants.MsgTradeStatusInvalid, nil)
		}
		now := time.Now()
		return s.orders.Transaction(ctx, func(txCtx context.Context) error {
			// Lock the order on its PRIMARY key first so every concurrent
			// transition acquires locks in the same order.
			locked, err := s.orders.FindByIDForUpdate(txCtx, orderID)
			if err != nil {
				return err
			}
			if locked.Status != constants.TradeStatusConfirmed {
				return util.ErrConflict
			}
			if err := s.orders.UpdateSellerConfirmed(txCtx, orderID, now); err != nil {
				return err
			}
			// A reserved product becomes sold; an already-sold product must not
			// be rewritten by a repeated confirmation (CAS returns ErrConflict).
			if err := s.products.UpdateStatusIfFrom(txCtx, order.ProductID,
				[]string{constants.ProductStatusReserved}, constants.ProductStatusSold); err != nil {
				return err
			}
			return nil
		})
	})
	if err != nil {
		if isConflictError(err) {
			return nil, conflictAppError(err)
		}
		if appErr := preserveAppError(err); appErr != nil {
			return nil, appErr
		}
		s.logger.Error(fmt.Sprintf(constants.LogTradeOrderCompleteFailed, orderID, err))
		return nil, util.WrapAppError(fmt.Errorf("trade_order[id=%d] seller confirm: %w", orderID, err), 500, constants.CodeInternalError, constants.MsgInternalError)
	}
	result, err := s.orders.FindByID(ctx, orderID)
	if err != nil {
		return nil, util.WrapAppError(fmt.Errorf("trade_order[id=%d] seller confirm reload: %w", orderID, err), 500, constants.CodeInternalError, constants.MsgInternalError)
	}
	s.logger.Info(fmt.Sprintf(constants.LogTradeOrderSellerConfirmSuccess, orderID))
	s.logger.Info(fmt.Sprintf(constants.LogTradeOrderCompleteSuccess, orderID, result.ProductID))
	s.logger.Info(fmt.Sprintf(constants.LogProductSoldSuccess, result.ProductID))
	return result, nil
}

// Cancel cancels an unfinished order (pending or confirmed) by either party
// and returns the product to on_sale atomically. The order PK row is locked
// before the compare-and-set updates so cancellation cannot deadlock against a
// concurrent confirmation; only one of the two can win.
func (s *TradeOrderService) Cancel(ctx context.Context, userID, orderID uint) (*model.TradeOrder, error) {
	err := withDeadlockRetry(func() error {
		order, err := s.orders.FindByID(ctx, orderID)
		if err != nil {
			return util.WrapAppError(fmt.Errorf("trade_order[id=%d] cancel find: %w", orderID, err), 404, constants.CodeNotFound, constants.MsgNotFound)
		}
		if order.BuyerID != userID && order.SellerID != userID {
			return util.NewAppError(403, constants.CodeForbidden, constants.MsgNotParticipant, nil)
		}
		if order.Status == constants.TradeStatusCompleted || order.Status == constants.TradeStatusCancelled {
			return util.NewAppError(409, constants.CodeConflict, constants.MsgTradeStatusInvalid, nil)
		}
		return s.orders.Transaction(ctx, func(txCtx context.Context) error {
			locked, err := s.orders.FindByIDForUpdate(txCtx, orderID)
			if err != nil {
				return err
			}
			active := false
			for _, st := range constants.CancellableTradeStatuses {
				if locked.Status == st {
					active = true
					break
				}
			}
			if !active {
				return util.ErrConflict
			}
			if err := s.orders.UpdateCancelledFrom(txCtx, orderID, constants.CancellableTradeStatuses); err != nil {
				return err
			}
			if err := s.products.UpdateStatusIfFrom(txCtx, order.ProductID,
				[]string{constants.ProductStatusReserved}, constants.ProductStatusOnSale); err != nil {
				return err
			}
			return nil
		})
	})
	if err != nil {
		if isConflictError(err) {
			return nil, conflictAppError(err)
		}
		if appErr := preserveAppError(err); appErr != nil {
			return nil, appErr
		}
		return nil, util.WrapAppError(fmt.Errorf("trade_order[id=%d] cancel: %w", orderID, err), 500, constants.CodeInternalError, constants.MsgInternalError)
	}
	result, err := s.orders.FindByID(ctx, orderID)
	if err != nil {
		return nil, util.WrapAppError(fmt.Errorf("trade_order[id=%d] cancel reload: %w", orderID, err), 500, constants.CodeInternalError, constants.MsgInternalError)
	}
	s.logger.Info(fmt.Sprintf(constants.LogProductRestoreOnSaleSuccess, result.ProductID, orderID))
	s.logger.Info(fmt.Sprintf(constants.LogTradeOrderCancelSuccess, orderID, result.ProductID))
	return result, nil
}
