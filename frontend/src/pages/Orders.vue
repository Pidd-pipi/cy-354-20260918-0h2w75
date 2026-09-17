<template>
  <div class="page">
    <h2>我的交易</h2>
    <el-card v-for="o in orders" :key="o.id" class="order-card">
      <div class="order-row">
        <div>
          <TradeStatusBadge :status="o.status" />
          <span class="order-id">订单 #{{ o.id }} · 商品 #{{ o.product_id }}</span>
          <p class="order-product" v-if="o.product">
            <el-tag :type="productStatusType(o.product.status) as any" size="small" effect="plain">
              {{ productStatusLabel(o.product.status) }}
            </el-tag>
            <span class="order-product-title">{{ o.product.title }}</span>
            <span class="order-product-price">¥{{ o.product.price.toFixed(2) }}</span>
          </p>
          <p class="order-meta">
            买家 #{{ o.buyer_id }} / 卖家 #{{ o.seller_id }} · {{ formatDateTime(o.created_at) }}
          </p>
        </div>
        <div class="order-actions">
          <el-button v-if="o.status === 'pending' && o.buyer_id === meId" size="small" type="primary" @click="onBuyerConfirm(o.id)">确认收货</el-button>
          <el-button v-if="o.status === 'confirmed' && o.seller_id === meId" size="small" type="success" @click="onSellerConfirm(o.id)">确认收款</el-button>
          <el-button v-if="canCancel(o)" size="small" type="danger" @click="onCancel(o.id)">取消订单</el-button>
          <el-button v-if="o.status === 'completed'" size="small" @click="reviewDialog(o)">评价</el-button>
        </div>
      </div>
    </el-card>
    <el-empty v-if="orders.length === 0" description="暂无交易" />
    <el-dialog v-model="reviewVisible" title="信誉评价" width="420px">
      <el-form label-width="70px">
        <el-form-item label="评价">
          <el-select v-model="reviewForm.rating" style="width: 100%">
            <el-option v-for="r in REVIEW_RATINGS" :key="r.value" :label="r.label" :value="r.value" />
          </el-select>
        </el-form-item>
        <el-form-item label="内容">
          <el-input v-model="reviewForm.content" type="textarea" :rows="3" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="reviewVisible = false">取消</el-button>
        <el-button type="primary" @click="submitReview">提交</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<script setup lang="ts">
import { computed, onActivated, onMounted, reactive, ref } from 'vue'
import { storeToRefs } from 'pinia'
import { ElMessage, ElMessageBox } from 'element-plus'
import TradeStatusBadge from '../components/common/TradeStatusBadge.vue'
import { useTradeStore } from '../stores/tradeStore'
import { useAuthStore } from '../stores/authStore'
import { buyerConfirm, sellerConfirm, cancelTradeOrder } from '../api/tradeOrder'
import { createReview } from '../api/review'
import { REVIEW_RATINGS } from '../constants/trade'
import { productStatusLabel, productStatusType } from '../constants/product'
import { formatDateTime } from '../utils/dateFormat'
import type { TradeOrder } from '../types'

const tradeStore = useTradeStore()
// 状态必须经 storeToRefs 解构，否则拿到的是首帧空数组快照，刷新后不会更新
const { orders } = storeToRefs(tradeStore)
const { fetch } = tradeStore
const authStore = useAuthStore()
const reviewVisible = ref(false)
const reviewForm = reactive({ trade_id: 0, rating: 'good', content: '' })

const meId = computed(() => authStore.user?.id ?? -1)

// 未完成（待确认/已确认）的订单，买卖双方均可取消；卖家确认收款后即完成，
// 不再允许取消。
function canCancel(o: TradeOrder): boolean {
  if (o.status !== 'pending' && o.status !== 'confirmed') {
    return false
  }
  return o.buyer_id === meId.value || o.seller_id === meId.value
}

async function onBuyerConfirm(id: number) {
  await buyerConfirm(id)
  ElMessage.success('已确认收货，等待卖家确认收款')
  await fetch()
}

async function onSellerConfirm(id: number) {
  await sellerConfirm(id)
  ElMessage.success('交易完成，商品已售出')
  await fetch()
}

async function onCancel(id: number) {
  try {
    await ElMessageBox.confirm('取消后商品将重新回到在售，确定取消该订单？', '取消订单', {
      type: 'warning',
      confirmButtonText: '确定取消',
      cancelButtonText: '再想想',
    })
  } catch {
    return
  }
  await cancelTradeOrder(id)
  ElMessage.success('订单已取消，商品已重新上架')
  await fetch()
}

function reviewDialog(o: TradeOrder) {
  reviewForm.trade_id = o.id
  reviewForm.rating = 'good'
  reviewForm.content = ''
  reviewVisible.value = true
}

async function submitReview() {
  await createReview({ trade_id: reviewForm.trade_id, rating: reviewForm.rating, content: reviewForm.content })
  ElMessage.success('评价成功')
  reviewVisible.value = false
  await fetch()
}

// 进入页面及从 keep-alive 切回时重新拉取，保证刷新后订单与商品状态一致。
onMounted(fetch)
onActivated(fetch)
</script>

<style scoped>
.order-card {
  margin-bottom: 12px;
}
.order-row {
  display: flex;
  justify-content: space-between;
  align-items: center;
}
.order-id {
  margin-left: 8px;
  font-size: 13px;
  color: #606266;
}
.order-product {
  margin: 6px 0 0;
  font-size: 13px;
  display: flex;
  align-items: center;
  gap: 8px;
}
.order-product-title {
  color: #303133;
}
.order-product-price {
  color: #f56c6c;
  font-weight: 600;
}
.order-meta {
  color: #909399;
  font-size: 12px;
  margin: 6px 0 0;
}
</style>
