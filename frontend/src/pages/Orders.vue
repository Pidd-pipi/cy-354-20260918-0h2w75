<template>
  <div class="page">
    <h2>我的交易</h2>
    <el-card v-for="o in tradeStore.orders" :key="o.id" class="order-card">
      <div class="order-row">
        <div>
          <TradeStatusBadge :status="o.status" />
          <span class="order-id">订单 #{{ o.id }} · 商品 #{{ o.product_id }}</span>
          <p class="order-meta">
            买家 #{{ o.buyer_id }} / 卖家 #{{ o.seller_id }} · {{ formatDateTime(o.created_at) }}
          </p>
        </div>
        <div class="order-actions">
          <el-button v-if="o.status === 'pending' && o.buyer_id === uid" size="small" type="primary" :loading="acting === o.id" @click="buyerConfirmFn(o.id)">确认收货</el-button>
          <el-button v-if="o.status === 'confirmed' && o.seller_id === uid" size="small" type="success" :loading="acting === o.id" @click="sellerConfirmFn(o.id)">确认收款</el-button>
          <el-button v-if="canCancel(o)" size="small" type="danger" plain :loading="acting === o.id" @click="cancelFn(o.id)">取消交易</el-button>
          <el-button v-if="o.status === 'completed'" size="small" @click="reviewDialog(o)">评价</el-button>
        </div>
      </div>
    </el-card>
    <el-empty v-if="tradeStore.orders.length === 0" description="暂无交易" />
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
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import TradeStatusBadge from '../components/common/TradeStatusBadge.vue'
import { useTradeStore } from '../stores/tradeStore'
import { useAuthStore } from '../stores/authStore'
import { useProductStore } from '../stores/productStore'
import { buyerConfirm, sellerConfirm, cancelTradeOrder } from '../api/tradeOrder'
import { createReview } from '../api/review'
import { REVIEW_RATINGS } from '../constants/trade'
import { formatDateTime } from '../utils/dateFormat'
import type { TradeOrder } from '../types'

const tradeStore = useTradeStore()
const { fetch } = tradeStore
const authStore = useAuthStore()
const productStore = useProductStore()
const reviewVisible = ref(false)
const acting = ref(0)
const reviewForm = reactive({ trade_id: 0, rating: 'good', content: '' })

const uid = computed(() => authStore.user?.id ?? 0)

// 未完成（待确认/已确认）前，买卖双方任一人都可以取消；完成或已取消则不可。
function canCancel(o: TradeOrder): boolean {
  if (o.status !== 'pending' && o.status !== 'confirmed') return false
  return o.buyer_id === uid.value || o.seller_id === uid.value
}

// 操作后同时刷新订单与商品列表，保证“我的交易”和商品广场的状态一致
// （reserved/on_sale/sold 联动）。
async function refreshAll() {
  await Promise.all([fetch(), productStore.fetch()])
}

async function buyerConfirmFn(id: number) {
  acting.value = id
  try {
    await buyerConfirm(id)
    ElMessage.success('已确认收货，等待卖家确认收款')
    await refreshAll()
  } finally {
    acting.value = 0
  }
}

async function sellerConfirmFn(id: number) {
  acting.value = id
  try {
    await sellerConfirm(id)
    ElMessage.success('交易完成，商品已售出')
    await refreshAll()
  } finally {
    acting.value = 0
  }
}

async function cancelFn(id: number) {
  try {
    await ElMessageBox.confirm('确定取消该交易吗？取消后商品将重新变为在售。', '取消交易', {
      confirmButtonText: '确定取消',
      cancelButtonText: '再想想',
      type: 'warning',
    })
  } catch {
    return
  }
  acting.value = id
  try {
    await cancelTradeOrder(id)
    ElMessage.success('交易已取消，商品重新上架')
    await refreshAll()
  } finally {
    acting.value = 0
  }
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

onMounted(fetch)
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
.order-meta {
  color: #909399;
  font-size: 12px;
  margin: 6px 0 0;
}
</style>
