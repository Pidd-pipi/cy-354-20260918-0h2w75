// Package integration contains end-to-end trade state machine tests that run
// against a real MySQL-compatible server (MySQL 8 / MariaDB 10.11+). They are
// skipped unless TEST_MYSQL_DSN is set, e.g.:
//
//	TEST_MYSQL_DSN='campus:campuspwd@tcp(127.0.0.1:3399)/campus_test?charset=utf8mb4&parseTime=True&loc=Local' \
//	  go test ./test/integration/ -v -count=1
package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lp/campus-market/internal/constants"
	"github.com/lp/campus-market/internal/dto"
	"github.com/lp/campus-market/internal/model"
	"github.com/lp/campus-market/internal/repository"
	"github.com/lp/campus-market/internal/service"
	"github.com/lp/campus-market/internal/util"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

type fixture struct {
	db       *gorm.DB
	orders   *repository.TradeOrderRepository
	products *repository.ProductRepository
	svc      *service.TradeOrderService
	logger   *slog.Logger
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN not set; skipping MySQL integration test")
	}
	// Every test run gets a clean database: drop and recreate every table from
	// the real database/init.sql DDL. The test user needs ALL privileges on
	// the test schema (DROP/CREATE/TEMPORARY).
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Warn)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(25)
	t.Cleanup(func() { _ = sqlDB.Close() })

	if err := resetSchema(t, db); err != nil {
		t.Fatalf("reset schema: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	orders := repository.NewTradeOrderRepository(db)
	products := repository.NewProductRepository(db)
	return &fixture{
		db:       db,
		orders:   orders,
		products: products,
		svc:      service.NewTradeOrderService(orders, products, logger),
		logger:   logger,
	}
}

// resetSchema drops all tables and replays the DDL section of database/init.sql
// (everything before the seed-data comment), with the database name adapted.
func resetSchema(t *testing.T, db *gorm.DB) error {
	initSQL, err := os.ReadFile(filepath.Join("..", "..", "..", "database", "init.sql"))
	if err != nil {
		return err
	}
	ddl := string(initSQL)
	ddl = strings.ReplaceAll(ddl, "lpcampusmarket_db", "campus_test")
	if idx := strings.Index(ddl, "-- 种子账号"); idx >= 0 {
		ddl = ddl[:idx]
	}
	stmts := strings.Split(ddl, ";")
	// drop in reverse creation order to avoid FK noise during re-runs
	for _, tbl := range []string{"reviews", "messages", "conversations", "trade_orders", "book_exchanges", "products", "users"} {
		_ = db.Exec("DROP TABLE IF EXISTS " + tbl).Error
	}
	for _, stmt := range stmts {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" || strings.HasPrefix(stmt, "--") || strings.HasPrefix(stmt, "USE ") {
			continue
		}
		if err := db.Exec(stmt).Error; err != nil {
			return fmt.Errorf("exec [%s]: %w", truncate(stmt, 80), err)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// seedUsersProduct creates seller + nBuyers buyers and one on-sale product.
func (f *fixture) seedUsersProduct(t *testing.T, nBuyers int) (seller uint, buyers []uint, productID uint) {
	t.Helper()
	seq := uint64(time.Now().UnixNano() % 1e10)
	mk := func(tag string) *model.User {
		u := &model.User{Phone: fmt.Sprintf("15%010d", atomic.AddUint64(&seq, 1)%1e10), PasswordHash: "x", Nickname: tag, Role: constants.UserRoleStudent, Campus: "东校区"}
		if err := f.db.Create(u).Error; err != nil {
			t.Fatalf("create user: %v", err)
		}
		return u
	}
	s := mk("seller")
	for i := 0; i < nBuyers; i++ {
		buyers = append(buyers, mk(fmt.Sprintf("buyer-%d", i)).ID)
	}
	p := &model.Product{
		SellerID: s.ID, Title: "并发测试商品", Price: 99, Category: constants.ProductCategoryBooks,
		Condition: "全新", Campus: "东校区", TradeLocation: "东门", Status: constants.ProductStatusOnSale,
	}
	if err := f.db.Create(p).Error; err != nil {
		t.Fatalf("create product: %v", err)
	}
	return s.ID, buyers, p.ID
}

func (f *fixture) user(id uint) *model.User { return &model.User{ID: id} }

func isConflict(err error) bool {
	var appErr *util.AppError
	return errors.As(err, &appErr) && appErr.Status == 409
}

// TestConcurrentBuyOnlyOneWins fires 10 simultaneous purchase requests for one
// product. Exactly one must succeed, the product must be reserved and there
// must be exactly one active order with no duplicate rows.
func TestConcurrentBuyOnlyOneWins(t *testing.T) {
	f := setup(t)
	_, buyers, productID := f.seedUsersProduct(t, 10)

	var success int64
	var wg sync.WaitGroup
	errs := make([]error, len(buyers))
	start := make(chan struct{})
	for i, buyerID := range buyers {
		wg.Add(1)
		go func(i int, buyerID uint) {
			defer wg.Done()
			<-start
			_, errs[i] = f.svc.Create(context.Background(), f.user(buyerID), &dto.CreateTradeOrderRequest{ProductID: productID})
			if errs[i] == nil {
				atomic.AddInt64(&success, 1)
			}
		}(i, buyerID)
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt64(&success); got != 1 {
		t.Fatalf("expected exactly 1 successful purchase, got %d", got)
	}
	var conflicts, others int
	for _, err := range errs {
		switch {
		case err == nil:
		case isConflict(err):
			conflicts++
		default:
			others++
			t.Errorf("unexpected error for a losing buyer: %v", err)
		}
	}
	if conflicts != 9 || others != 0 {
		t.Fatalf("expected 9 conflict failures, got conflicts=%d others=%d", conflicts, others)
	}

	p, err := f.products.FindByID(context.Background(), productID)
	if err != nil {
		t.Fatalf("reload product: %v", err)
	}
	if p.Status != constants.ProductStatusReserved {
		t.Fatalf("expected product reserved, got %s", p.Status)
	}
	active, err := f.orders.CountActiveByProduct(context.Background(), productID)
	if err != nil {
		t.Fatalf("count active: %v", err)
	}
	if active != 1 {
		t.Fatalf("expected exactly 1 active order, got %d", active)
	}
	var total int64
	f.db.Model(&model.TradeOrder{}).Where("product_id = ?", productID).Count(&total)
	if total != 1 {
		t.Fatalf("expected exactly 1 order row (no duplicates), got %d", total)
	}
}

// TestSameBuyerCannotOrderTwice verifies the same buyer gets a clear 409 on a
// repeated click and that the second attempt leaves no duplicate order.
func TestSameBuyerCannotOrderTwice(t *testing.T) {
	f := setup(t)
	_, buyers, productID := f.seedUsersProduct(t, 1)
	req := &dto.CreateTradeOrderRequest{ProductID: productID}

	if _, err := f.svc.Create(context.Background(), f.user(buyers[0]), req); err != nil {
		t.Fatalf("first purchase: %v", err)
	}
	_, err := f.svc.Create(context.Background(), f.user(buyers[0]), req)
	if !isConflict(err) {
		t.Fatalf("expected 409 on duplicate purchase, got %v", err)
	}
	active, _ := f.orders.CountActiveByProduct(context.Background(), productID)
	if active != 1 {
		t.Fatalf("expected 1 active order, got %d", active)
	}
}

// TestBuyOwnProductRejected is a guard against self-dealing orders.
func TestBuyOwnProductRejected(t *testing.T) {
	f := setup(t)
	seller, _, productID := f.seedUsersProduct(t, 0)
	_, err := f.svc.Create(context.Background(), f.user(seller), &dto.CreateTradeOrderRequest{ProductID: productID})
	var appErr *util.AppError
	if !errors.As(err, &appErr) || appErr.Status != 400 {
		t.Fatalf("expected 400 buying own product, got %v", err)
	}
}

// TestBuyerCancelRollsProductBackToOnSale covers the cancel rollback while the
// order is pending, and proves the product can be re-purchased afterwards.
func TestBuyerCancelRollsProductBackToOnSale(t *testing.T) {
	f := setup(t)
	_, buyers, productID := f.seedUsersProduct(t, 2)
	ctx := context.Background()

	order, err := f.svc.Create(ctx, f.user(buyers[0]), &dto.CreateTradeOrderRequest{ProductID: productID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.svc.Cancel(ctx, buyers[0], order.ID); err != nil {
		t.Fatalf("buyer cancel: %v", err)
	}
	f.assertProductStatus(t, productID, constants.ProductStatusOnSale)
	f.assertOrderStatus(t, order.ID, constants.TradeStatusCancelled)

	// another buyer can now purchase the same product successfully
	order2, err := f.svc.Create(ctx, f.user(buyers[1]), &dto.CreateTradeOrderRequest{ProductID: productID})
	if err != nil {
		t.Fatalf("re-purchase after cancel: %v", err)
	}
	f.assertProductStatus(t, productID, constants.ProductStatusReserved)
	active, _ := f.orders.CountActiveByProduct(ctx, productID)
	if active != 1 {
		t.Fatalf("expected 1 active order after re-purchase, got %d", active)
	}
	if order2.ID == order.ID {
		t.Fatalf("re-purchase created no new order")
	}
}

// TestSellerCancelAfterBuyerConfirmRollsBack covers cancellation from the
// "confirmed" state (buyer received goods, seller has not confirmed payment):
// either party may cancel and the product returns on_sale.
func TestSellerCancelAfterBuyerConfirmRollsBack(t *testing.T) {
	f := setup(t)
	seller, buyers, productID := f.seedUsersProduct(t, 1)
	ctx := context.Background()

	order, err := f.svc.Create(ctx, f.user(buyers[0]), &dto.CreateTradeOrderRequest{ProductID: productID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := f.svc.BuyerConfirm(ctx, buyers[0], order.ID); err != nil {
		t.Fatalf("buyer confirm: %v", err)
	}
	if _, err := f.svc.SellerConfirm(ctx, buyers[0], order.ID); err == nil {
		t.Fatalf("buyer must not be able to seller-confirm")
	}
	// the seller cancels before confirming payment
	if _, err := f.svc.Cancel(ctx, seller, order.ID); err != nil {
		t.Fatalf("seller cancel in confirmed state: %v", err)
	}
	f.assertOrderStatus(t, order.ID, constants.TradeStatusCancelled)
	f.assertProductStatus(t, productID, constants.ProductStatusOnSale)
}

// TestFullHappyFlow verifies pending -> buyer confirm -> seller confirm ->
// completed and the product ends up sold.
func TestFullHappyFlow(t *testing.T) {
	f := setup(t)
	seller, buyers, productID := f.seedUsersProduct(t, 1)
	ctx := context.Background()

	order, err := f.svc.Create(ctx, f.user(buyers[0]), &dto.CreateTradeOrderRequest{ProductID: productID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	f.assertProductStatus(t, productID, constants.ProductStatusReserved)

	// seller cannot confirm payment before buyer confirms receipt
	if _, err := f.svc.SellerConfirm(ctx, seller, order.ID); !isConflict(err) {
		t.Fatalf("expected 409 seller-confirm before buyer-confirm, got %v", err)
	}
	if _, err := f.svc.BuyerConfirm(ctx, seller, order.ID); err == nil {
		t.Fatalf("seller must not trigger buyer confirm")
	}

	if _, err := f.svc.BuyerConfirm(ctx, buyers[0], order.ID); err != nil {
		t.Fatalf("buyer confirm: %v", err)
	}
	f.assertOrderStatus(t, order.ID, constants.TradeStatusConfirmed)
	f.assertProductStatus(t, productID, constants.ProductStatusReserved)

	completed, err := f.svc.SellerConfirm(ctx, seller, order.ID)
	if err != nil {
		t.Fatalf("seller confirm: %v", err)
	}
	if completed.Status != constants.TradeStatusCompleted || completed.SellerConfirmedAt == nil || completed.CompletedAt == nil {
		t.Fatalf("order not completed correctly: %+v", completed)
	}
	f.assertProductStatus(t, productID, constants.ProductStatusSold)
}

// TestDuplicateConfirmationsRejected repeats every confirmation action and
// checks that double clicks / out-of-order actions never corrupt the state.
func TestDuplicateConfirmationsRejected(t *testing.T) {
	f := setup(t)
	seller, buyers, productID := f.seedUsersProduct(t, 1)
	ctx := context.Background()

	order, _ := f.svc.Create(ctx, f.user(buyers[0]), &dto.CreateTradeOrderRequest{ProductID: productID})

	if _, err := f.svc.BuyerConfirm(ctx, buyers[0], order.ID); err != nil {
		t.Fatalf("first buyer confirm: %v", err)
	}
	// repeated buyer confirm fails
	if _, err := f.svc.BuyerConfirm(ctx, buyers[0], order.ID); !isConflict(err) {
		t.Fatalf("duplicate buyer confirm should 409, got %v", err)
	}
	if _, err := f.svc.SellerConfirm(ctx, seller, order.ID); err != nil {
		t.Fatalf("seller confirm: %v", err)
	}
	// repeated seller confirm fails and does not touch the sold product
	if _, err := f.svc.SellerConfirm(ctx, seller, order.ID); !isConflict(err) {
		t.Fatalf("duplicate seller confirm should 409, got %v", err)
	}
	// cancel after completion fails
	if _, err := f.svc.Cancel(ctx, buyers[0], order.ID); !isConflict(err) {
		t.Fatalf("cancel after completion should 409, got %v", err)
	}
	if _, err := f.svc.Cancel(ctx, seller, order.ID); !isConflict(err) {
		t.Fatalf("seller cancel after completion should 409, got %v", err)
	}
	// double cancel on a still-pending order: second call must fail too
	_, buyers2, productID2 := f.seedUsersProductFromSeller(t, seller, 2)
	order2, _ := f.svc.Create(ctx, f.user(buyers2[0]), &dto.CreateTradeOrderRequest{ProductID: productID2})
	if _, err := f.svc.Cancel(ctx, buyers2[0], order2.ID); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	if _, err := f.svc.Cancel(ctx, buyers2[0], order2.ID); !isConflict(err) {
		t.Fatalf("duplicate cancel should 409, got %v", err)
	}
	f.assertProductStatus(t, productID2, constants.ProductStatusOnSale)
	f.assertProductStatus(t, productID, constants.ProductStatusSold)
}

// TestConcurrentCancelVsSellerConfirm races cancel and seller confirm on a
// confirmed order. Exactly one outcome is legal and cross-table state must be
// consistent in either case.
func TestConcurrentCancelVsSellerConfirm(t *testing.T) {
	f := setup(t)
	seller, buyers, productID := f.seedUsersProduct(t, 1)
	ctx := context.Background()

	order, _ := f.svc.Create(ctx, f.user(buyers[0]), &dto.CreateTradeOrderRequest{ProductID: productID})
	if _, err := f.svc.BuyerConfirm(ctx, buyers[0], order.ID); err != nil {
		t.Fatalf("buyer confirm: %v", err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	var cancelOK, confirmOK int64
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		if _, err := f.svc.Cancel(ctx, buyers[0], order.ID); err == nil {
			atomic.AddInt64(&cancelOK, 1)
		} else if !isConflict(err) {
			t.Errorf("unexpected cancel error: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		if _, err := f.svc.SellerConfirm(ctx, seller, order.ID); err == nil {
			atomic.AddInt64(&confirmOK, 1)
		} else if !isConflict(err) {
			t.Errorf("unexpected seller confirm error: %v", err)
		}
	}()
	close(start)
	wg.Wait()

	if atomic.LoadInt64(&cancelOK)+atomic.LoadInt64(&confirmOK) != 1 {
		t.Fatalf("exactly one of cancel/confirm must win, got cancel=%d confirm=%d", cancelOK, confirmOK)
	}
	gotOrder, err := f.orders.FindByID(ctx, order.ID)
	if err != nil {
		t.Fatalf("reload order: %v", err)
	}
	gotProduct, _ := f.products.FindByID(ctx, productID)
	switch gotOrder.Status {
	case constants.TradeStatusCompleted:
		if gotProduct.Status != constants.ProductStatusSold {
			t.Fatalf("completed order but product=%s", gotProduct.Status)
		}
	case constants.TradeStatusCancelled:
		if gotProduct.Status != constants.ProductStatusOnSale {
			t.Fatalf("cancelled order but product=%s", gotProduct.Status)
		}
	default:
		t.Fatalf("illegal terminal order status %s", gotOrder.Status)
	}
}

// TestOutsiderCannotOperate ensures non-participants cannot confirm or cancel.
func TestOutsiderCannotOperate(t *testing.T) {
	f := setup(t)
	_, buyers, productID := f.seedUsersProduct(t, 2)
	ctx := context.Background()
	order, _ := f.svc.Create(ctx, f.user(buyers[0]), &dto.CreateTradeOrderRequest{ProductID: productID})

	if _, err := f.svc.BuyerConfirm(ctx, buyers[1], order.ID); err == nil {
		t.Fatalf("outsider buyer-confirm should fail")
	}
	if _, err := f.svc.SellerConfirm(ctx, buyers[1], order.ID); err == nil {
		t.Fatalf("outsider seller-confirm should fail")
	}
	if _, err := f.svc.Cancel(ctx, buyers[1], order.ID); err == nil {
		t.Fatalf("outsider cancel should fail")
	}
	f.assertOrderStatus(t, order.ID, constants.TradeStatusPending)
	f.assertProductStatus(t, productID, constants.ProductStatusReserved)
}

func (f *fixture) assertProductStatus(t *testing.T, productID uint, want string) {
	t.Helper()
	p, err := f.products.FindByID(context.Background(), productID)
	if err != nil {
		t.Fatalf("load product %d: %v", productID, err)
	}
	if p.Status != want {
		t.Fatalf("product %d status = %s, want %s", productID, p.Status, want)
	}
}

func (f *fixture) assertOrderStatus(t *testing.T, orderID uint, want string) {
	t.Helper()
	o, err := f.orders.FindByID(context.Background(), orderID)
	if err != nil {
		t.Fatalf("load order %d: %v", orderID, err)
	}
	if o.Status != want {
		t.Fatalf("order %d status = %s, want %s", orderID, o.Status, want)
	}
}

// seedUsersProductFromSeller reuses an existing seller (FK-less, but keeps the
// story consistent) and creates a fresh product plus buyers.
func (f *fixture) seedUsersProductFromSeller(t *testing.T, seller uint, nBuyers int) (uint, []uint, uint) {
	t.Helper()
	var buyers []uint
	for i := 0; i < nBuyers; i++ {
		u := &model.User{Phone: fmt.Sprintf("16%010d", i+1), PasswordHash: "x", Nickname: "b", Role: constants.UserRoleStudent}
		if err := f.db.Create(u).Error; err != nil {
			t.Fatalf("create buyer: %v", err)
		}
		buyers = append(buyers, u.ID)
	}
	p := &model.Product{SellerID: seller, Title: "重复确认测试商品", Price: 5, Category: constants.ProductCategoryDaily, Status: constants.ProductStatusOnSale}
	if err := f.db.Create(p).Error; err != nil {
		t.Fatalf("create product: %v", err)
	}
	return seller, buyers, p.ID
}
