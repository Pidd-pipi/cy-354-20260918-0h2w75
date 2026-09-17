package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/lp/campus-market/internal/constants"
	"github.com/lp/campus-market/internal/dto"
	"github.com/lp/campus-market/internal/model"
	"github.com/lp/campus-market/internal/repository"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// integrationDSN returns the MySQL DSN used by the lifecycle tests. Set
// TEST_DB_DSN to run them; tests skip when the database is unreachable.
func integrationDSN() string {
	if dsn := os.Getenv("TEST_DB_DSN"); dsn != "" {
		return dsn
	}
	return "lpcampusmarket_user:lpcampusmarket_pwd@tcp(127.0.0.1:3306)/lpcampusmarket_db?charset=utf8mb4&parseTime=True&loc=Local"
}

func newIntegrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(mysql.Open(integrationDSN()), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Skipf("integration database unavailable: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Skipf("integration database pool unavailable: %v", err)
	}
	sqlDB.SetMaxOpenConns(20)
	if err := sqlDB.Ping(); err != nil {
		t.Skipf("integration database ping failed: %v", err)
	}
	if err := db.AutoMigrate(
		model.User{}, model.Product{}, model.Conversation{}, model.Message{},
		model.TradeOrder{}, model.Review{}, model.BookExchange{},
	); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	if err := repository.EnsureTradeOrderInvariants(context.Background(), db); err != nil {
		t.Fatalf("ensure invariants: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// itSeq is a process-wide monotonic sequence keeping generated phones and
// titles unique across test runs and subtests sharing one database.
var itSeq atomic.Int64

// uniquePhone returns an unused 11-digit phone number for seed users.
func uniquePhone(prefix string) string {
	n := itSeq.Add(1)
	return fmt.Sprintf("%s%0*d", prefix, 11-len(prefix), n%100000000)
}

// uniqueSuffix produces a process-unique token for product titles.
func uniqueSuffix() string {
	return fmt.Sprintf("%d", itSeq.Add(1))
}

func seedUser(t *testing.T, db *gorm.DB, phone string) *model.User {
	t.Helper()
	// Clear any leftover row from an earlier interrupted run.
	db.Unscoped().Where("phone = ?", phone).Delete(&model.User{})
	u := &model.User{
		Phone: phone, PasswordHash: "x", Nickname: "it-" + phone,
		Role: constants.UserRoleStudent, Campus: "东校区", CreditScore: 100,
	}
	if err := db.Create(u).Error; err != nil {
		t.Fatalf("seed user %s: %v", phone, err)
	}
	t.Cleanup(func() { db.Unscoped().Delete(u) })
	return u
}

func seedProduct(t *testing.T, db *gorm.DB, sellerID uint) *model.Product {
	t.Helper()
	title := "it-product-" + uniqueSuffix()
	db.Unscoped().Where("title = ?", title).Delete(&model.Product{})
	p := &model.Product{
		SellerID: sellerID, Title: title, Description: "集成测试商品",
		Price: 9.9, Category: constants.ProductCategoryBooks, Condition: "全新",
		Campus: "东校区", TradeLocation: "东门", Status: constants.ProductStatusOnSale,
	}
	if err := db.Create(p).Error; err != nil {
		t.Fatalf("seed product: %v", err)
	}
	t.Cleanup(func() { db.Unscoped().Delete(p) })
	return p
}

func newTradeServices(db *gorm.DB) (*TradeOrderService, *repository.TradeOrderRepository, *repository.ProductRepository) {
	orders := repository.NewTradeOrderRepository(db)
	products := repository.NewProductRepository(db)
	return NewTradeOrderService(orders, products, slog.Default()), orders, products
}

func cleanupOrders(db *gorm.DB, productID uint) {
	db.Where("product_id = ?", productID).Delete(&model.TradeOrder{})
}

// TestIntegrationConcurrentPurchase fires many buyers at one product and
// requires exactly one pending order with the product flipped to reserved.
func TestIntegrationConcurrentPurchase(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	seller := seedUser(t, db, uniquePhone("139"))
	product := seedProduct(t, db, seller.ID)
	t.Cleanup(func() { cleanupOrders(db, product.ID) })

	svc, _, products := newTradeServices(db)

	const buyers = 12
	buyerUsers := make([]*model.User, buyers)
	for i := range buyerUsers {
		buyerUsers[i] = seedUser(t, db, uniquePhone("139"))
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, buyers)
	var successCount int64
	var mu sync.Mutex
	wg.Add(buyers)
	for i := 0; i < buyers; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			_, err := svc.Create(ctx, buyerUsers[idx], &dto.CreateTradeOrderRequest{ProductID: product.ID})
			errs[idx] = err
			if err == nil {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if successCount != 1 {
		t.Fatalf("expected exactly 1 successful purchase, got %d (errs=%v)", successCount, errs)
	}

	var activeCount int64
	if err := db.Model(&model.TradeOrder{}).
		Where("product_id = ? AND status IN ?", product.ID, []string{constants.TradeStatusPending, constants.TradeStatusConfirmed}).
		Count(&activeCount).Error; err != nil {
		t.Fatal(err)
	}
	if activeCount != 1 {
		t.Fatalf("expected exactly 1 active order row, got %d", activeCount)
	}

	var totalOrders int64
	if err := db.Model(&model.TradeOrder{}).Where("product_id = ?", product.ID).Count(&totalOrders).Error; err != nil {
		t.Fatal(err)
	}
	if totalOrders != 1 {
		t.Fatalf("expected no duplicate/leftover order rows, got %d", totalOrders)
	}

	got, err := products.FindByID(ctx, product.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != constants.ProductStatusReserved {
		t.Fatalf("expected product reserved, got %s", got.Status)
	}

	// One more sequential attempt must keep failing while reserved.
	extra := seedUser(t, db, uniquePhone("139"))
	if _, err := svc.Create(ctx, extra, &dto.CreateTradeOrderRequest{ProductID: product.ID}); err == nil {
		t.Fatal("expected repeated purchase to fail while reserved")
	}
}

// TestIntegrationRawInvariantBlocksDuplicate proves the database unique
// index alone rejects a second active order even if the service is bypassed.
func TestIntegrationRawInvariantBlocksDuplicate(t *testing.T) {
	db := newIntegrationDB(t)
	seller := seedUser(t, db, uniquePhone("138"))
	product := seedProduct(t, db, seller.ID)
	t.Cleanup(func() { cleanupOrders(db, product.ID) })

	first := model.TradeOrder{ProductID: product.ID, BuyerID: seller.ID + 1, SellerID: seller.ID, Status: constants.TradeStatusPending}
	second := model.TradeOrder{ProductID: product.ID, BuyerID: seller.ID + 2, SellerID: seller.ID, Status: constants.TradeStatusPending}
	if err := db.Create(&first).Error; err != nil {
		t.Fatalf("insert first order: %v", err)
	}
	err := db.Create(&second).Error
	if err == nil {
		t.Fatal("expected duplicate active order insert to fail")
	}
	var mysqlErr *mysqldriver.MySQLError
	if !errors.As(err, &mysqlErr) || mysqlErr.Number != 1062 {
		t.Fatalf("expected mysql duplicate entry error 1062, got %v", err)
	}

	// A cancelled row frees the slot: a new pending order is allowed and a
	// completed historical row can coexist with it.
	if err := db.Model(&model.TradeOrder{}).Where("id = ?", first.ID).
		Update("status", constants.TradeStatusCancelled).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&second).Error; err != nil {
		t.Fatalf("insert after cancel should succeed: %v", err)
	}
}

// TestIntegrationCancelRollback covers buyer/seller cancellation releasing
// the product back on sale and allowing re-purchase.
func TestIntegrationCancelRollback(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	svc, _, products := newTradeServices(db)

	t.Run("buyer cancel releases product", func(t *testing.T) {
		seller := seedUser(t, db, uniquePhone("137"))
		product := seedProduct(t, db, seller.ID)
		t.Cleanup(func() { cleanupOrders(db, product.ID) })
		buyer := seedUser(t, db, uniquePhone("136"))

		order, err := svc.Create(ctx, buyer, &dto.CreateTradeOrderRequest{ProductID: product.ID})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		got, _ := products.FindByID(ctx, product.ID)
		if got.Status != constants.ProductStatusReserved {
			t.Fatalf("expected reserved, got %s", got.Status)
		}

		if _, err := svc.Cancel(ctx, buyer.ID, order.ID); err != nil {
			t.Fatalf("buyer cancel: %v", err)
		}
		got, _ = products.FindByID(ctx, product.ID)
		if got.Status != constants.ProductStatusOnSale {
			t.Fatalf("expected on_sale after cancel, got %s", got.Status)
		}
		cancelled, _ := svc.orders.FindByID(ctx, order.ID)
		if cancelled.Status != constants.TradeStatusCancelled {
			t.Fatalf("expected cancelled order, got %s", cancelled.Status)
		}

		// Re-purchase after release succeeds and creates a fresh order.
		buyer2 := seedUser(t, db, uniquePhone("135"))
		order2, err := svc.Create(ctx, buyer2, &dto.CreateTradeOrderRequest{ProductID: product.ID})
		if err != nil {
			t.Fatalf("repurchase after cancel: %v", err)
		}
		if order2.ID == order.ID {
			t.Fatal("expected a new order after re-purchase")
		}
	})

	t.Run("seller cancel releases product", func(t *testing.T) {
		seller := seedUser(t, db, uniquePhone("134"))
		product := seedProduct(t, db, seller.ID)
		t.Cleanup(func() { cleanupOrders(db, product.ID) })
		buyer := seedUser(t, db, uniquePhone("133"))

		order, err := svc.Create(ctx, buyer, &dto.CreateTradeOrderRequest{ProductID: product.ID})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := svc.Cancel(ctx, seller.ID, order.ID); err != nil {
			t.Fatalf("seller cancel: %v", err)
		}
		got, _ := products.FindByID(ctx, product.ID)
		if got.Status != constants.ProductStatusOnSale {
			t.Fatalf("expected on_sale after seller cancel, got %s", got.Status)
		}
	})

	t.Run("duplicate cancel fails without changing product", func(t *testing.T) {
		seller := seedUser(t, db, uniquePhone("132"))
		product := seedProduct(t, db, seller.ID)
		t.Cleanup(func() { cleanupOrders(db, product.ID) })
		buyer := seedUser(t, db, uniquePhone("131"))

		order, err := svc.Create(ctx, buyer, &dto.CreateTradeOrderRequest{ProductID: product.ID})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := svc.Cancel(ctx, buyer.ID, order.ID); err != nil {
			t.Fatalf("first cancel: %v", err)
		}
		if _, err := svc.Cancel(ctx, buyer.ID, order.ID); err == nil {
			t.Fatal("expected duplicate cancel to fail")
		}
		if _, err := svc.Cancel(ctx, seller.ID, order.ID); err == nil {
			t.Fatal("expected counterparty late cancel to fail")
		}
		got, _ := products.FindByID(ctx, product.ID)
		if got.Status != constants.ProductStatusOnSale {
			t.Fatalf("product status must stay on_sale, got %s", got.Status)
		}
	})
}

// TestIntegrationConfirmSequence covers the buyer-receipt then
// seller-payment ordering, duplicate confirmations and final sold state.
func TestIntegrationConfirmSequence(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	svc, _, products := newTradeServices(db)

	seller := seedUser(t, db, uniquePhone("130"))
	product := seedProduct(t, db, seller.ID)
	t.Cleanup(func() { cleanupOrders(db, product.ID) })
	buyer := seedUser(t, db, uniquePhone("159"))

	order, err := svc.Create(ctx, buyer, &dto.CreateTradeOrderRequest{ProductID: product.ID})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Seller cannot confirm payment before buyer confirms receipt.
	if _, err := svc.SellerConfirm(ctx, seller.ID, order.ID); err == nil {
		t.Fatal("seller confirm before buyer receipt must fail")
	}
	// Only the buyer may confirm receipt.
	if _, err := svc.BuyerConfirm(ctx, seller.ID, order.ID); err == nil {
		t.Fatal("seller must not be able to buyer-confirm")
	}

	confirmed, err := svc.BuyerConfirm(ctx, buyer.ID, order.ID)
	if err != nil {
		t.Fatalf("buyer confirm: %v", err)
	}
	if confirmed.Status != constants.TradeStatusConfirmed || confirmed.BuyerConfirmedAt == nil {
		t.Fatalf("unexpected confirmed order: %+v", confirmed)
	}
	// Duplicate buyer confirmation fails.
	if _, err := svc.BuyerConfirm(ctx, buyer.ID, order.ID); err == nil {
		t.Fatal("duplicate buyer confirm must fail")
	}
	// Product stays reserved between the two confirmations.
	got, _ := products.FindByID(ctx, product.ID)
	if got.Status != constants.ProductStatusReserved {
		t.Fatalf("expected reserved mid-flow, got %s", got.Status)
	}
	// Only the seller may confirm payment.
	if _, err := svc.SellerConfirm(ctx, buyer.ID, order.ID); err == nil {
		t.Fatal("buyer must not be able to seller-confirm")
	}

	completed, err := svc.SellerConfirm(ctx, seller.ID, order.ID)
	if err != nil {
		t.Fatalf("seller confirm: %v", err)
	}
	if completed.Status != constants.TradeStatusCompleted || completed.CompletedAt == nil {
		t.Fatalf("unexpected completed order: %+v", completed)
	}
	got, _ = products.FindByID(ctx, product.ID)
	if got.Status != constants.ProductStatusSold {
		t.Fatalf("expected sold after completion, got %s", got.Status)
	}

	// Repeated seller confirmation and post-completion cancel both fail and
	// leave the product sold.
	if _, err := svc.SellerConfirm(ctx, seller.ID, order.ID); err == nil {
		t.Fatal("duplicate seller confirm must fail")
	}
	if _, err := svc.Cancel(ctx, buyer.ID, order.ID); err == nil {
		t.Fatal("cancel after completion must fail")
	}
	if _, err := svc.Cancel(ctx, seller.ID, order.ID); err == nil {
		t.Fatal("seller cancel after completion must fail")
	}
	got, _ = products.FindByID(ctx, product.ID)
	if got.Status != constants.ProductStatusSold {
		t.Fatalf("expected product to remain sold, got %s", got.Status)
	}

	// ListMy returns the order with the sold product snapshot attached.
	page, err := svc.ListMy(ctx, buyer.ID, &dto.PageQuery{})
	if err != nil {
		t.Fatalf("list my orders: %v", err)
	}
	var found bool
	for _, raw := range page.Items.([]model.TradeOrder) {
		if raw.ID == order.ID {
			found = true
			if raw.Product == nil || raw.Product.Status != constants.ProductStatusSold {
				t.Fatalf("expected attached sold product snapshot, got %+v", raw.Product)
			}
		}
	}
	if !found {
		t.Fatal("completed order missing from buyer list")
	}
}

// TestIntegrationCannotBuyOwnOrRemovedProduct guards the entry checks.
func TestIntegrationCannotBuyOwnOrRemovedProduct(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	svc, _, _ := newTradeServices(db)

	seller := seedUser(t, db, uniquePhone("158"))
	product := seedProduct(t, db, seller.ID)
	t.Cleanup(func() { cleanupOrders(db, product.ID) })

	if _, err := svc.Create(ctx, seller, &dto.CreateTradeOrderRequest{ProductID: product.ID}); err == nil {
		t.Fatal("buying own product must fail")
	}

	if err := db.Model(&model.Product{}).Where("id = ?", product.ID).
		Update("status", constants.ProductStatusRemoved).Error; err != nil {
		t.Fatal(err)
	}
	buyer := seedUser(t, db, uniquePhone("157"))
	if _, err := svc.Create(ctx, buyer, &dto.CreateTradeOrderRequest{ProductID: product.ID}); err == nil {
		t.Fatal("buying a removed product must fail")
	}
	var orderCount int64
	if err := db.Model(&model.TradeOrder{}).Where("product_id = ?", product.ID).Count(&orderCount).Error; err != nil {
		t.Fatal(err)
	}
	if orderCount != 0 {
		t.Fatalf("failed purchases must not leave orders, got %d", orderCount)
	}
}
