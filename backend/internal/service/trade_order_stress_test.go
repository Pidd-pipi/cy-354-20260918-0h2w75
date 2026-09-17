package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lp/campus-market/internal/constants"
	"github.com/lp/campus-market/internal/dto"
	"github.com/lp/campus-market/internal/model"
)

// TestIntegrationConcurrentPurchaseStress raises contention to 50 buyers
// hammering one product and requires exactly one surviving active order.
func TestIntegrationConcurrentPurchaseStress(t *testing.T) {
	db := newIntegrationDB(t)
	ctx := context.Background()
	seller := seedUser(t, db, uniquePhone("176"))
	product := seedProduct(t, db, seller.ID)
	t.Cleanup(func() { cleanupOrders(db, product.ID) })
	svc, _, products := newTradeServices(db)

	const n = 50
	buyers := make([]*model.User, n)
	for i := range buyers {
		buyers[i] = seedUser(t, db, uniquePhone("176"))
	}

	var okCount int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			if _, err := svc.Create(ctx, buyers[idx], &dto.CreateTradeOrderRequest{ProductID: product.ID}); err == nil {
				atomic.AddInt64(&okCount, 1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if okCount != 1 {
		t.Fatalf("expected 1 winner among 50, got %d", okCount)
	}
	var active int64
	db.Model(&model.TradeOrder{}).
		Where("product_id = ? AND status IN ?", product.ID, []string{constants.TradeStatusPending, constants.TradeStatusConfirmed}).
		Count(&active)
	if active != 1 {
		t.Fatalf("expected 1 active order, got %d", active)
	}
	got, _ := products.FindByID(ctx, product.ID)
	if got.Status != constants.ProductStatusReserved {
		t.Fatalf("expected reserved, got %s", got.Status)
	}
}
