<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import AppIcon from '../components/AppIcon.vue'
import MetricCard from '../components/MetricCard.vue'
import TrendChart from '../components/TrendChart.vue'
import { checkProviders } from '../api'
import { compact, duration, thousand } from '../chart'
import type { AppInfo, OverviewResponse, ProviderCheck, ProviderInfo } from '../types'

const props = defineProps<{
  overview: OverviewResponse | null
  app: AppInfo
  providers: ProviderInfo[]
  days: number
}>()

const emit = defineEmits<{ (e: 'reload', days: number): void }>()

const RANGES = [
  { label: '7 天', value: 7 },
  { label: '14 天', value: 14 },
  { label: '30 天', value: 30 },
]

const METRICS = [
  { key: 'calls', label: '调用次数' },
  { key: 'tokens', label: 'Token' },
] as const

const metric = ref<'calls' | 'tokens'>('calls')

const stats = computed(() => props.overview?.stats ?? null)
const daily = computed(() => stats.value?.daily ?? [])
const recent = computed(() => stats.value?.recent ?? [])
const hasData = computed(() => (stats.value?.total_calls ?? 0) > 0)

const callsSeries = computed(() => daily.value.map((d) => d.calls))
const tokensSeries = computed(() => daily.value.map((d) => d.tokens))
const successSeries = computed(() =>
  daily.value.map((d) => (d.calls ? ((d.calls - d.failures) / d.calls) * 100 : 0)),
)
const latencySeries = computed(() =>
  recent.value.map((r) => r.latency_ms).reverse(),
)

/** 最后一天相对前一天的环比（无前一天数据时返回 undefined）。 */
function deltaOf(series: number[]): number | undefined {
  if (series.length < 2) return undefined
  const prev = series[series.length - 2]
  const cur = series[series.length - 1]
  if (!prev) return undefined
  return ((cur - prev) / prev) * 100
}

const trendData = computed(() =>
  daily.value.map((d) => ({
    label: d.day.slice(5),
    value: metric.value === 'calls' ? d.calls : d.tokens,
  })),
)

const trendTotal = computed(() =>
  trendData.value.reduce((sum, d) => sum + d.value, 0),
)

const trendColor = computed(() => 'var(--accent)')

const successRateText = computed(() => {
  const s = stats.value
  if (!s || !s.total_calls) return '—'
  return `${(s.success_rate * 100).toFixed(1)}%`
})

const modelRows = computed(() => stats.value?.models ?? [])

const stepsHint = computed(() =>
  stats.value?.last_call_at ? `最近调用 ${stats.value.last_call_at}` : '暂无调用记录',
)

/* ==========================================================================
 * AI 端点连通性
 * 仅在「页面加载」时测一次 + 用户手动点击「测试 / 全部测试」时测一次，
 * 不做后台实时轮询（无定时器）。
 * ======================================================================== */
type Tone = 'ok' | 'warn' | 'off' | 'idle'

const checks = ref<Record<string, ProviderCheck>>({})
const checking = ref<Record<string, boolean>>({})
const checkingAll = ref(false)

const STATUS_VIEW: Record<string, { tone: Tone; label: string }> = {
  ok: { tone: 'ok', label: '在线' },
  auth_error: { tone: 'warn', label: '鉴权失败' },
  http_error: { tone: 'warn', label: '服务异常' },
  unreachable: { tone: 'off', label: '无法连接' },
  unknown_provider: { tone: 'off', label: '未声明' },
  error: { tone: 'off', label: '测试失败' },
}

/** 单个端点的展示状态（未测试 / 测试中 / 已测试）。 */
function statusOf(name: string): { tone: Tone; label: string; latency: number; detail: string } {
  if (checking.value[name]) {
    return { tone: 'idle', label: '测试中…', latency: 0, detail: '' }
  }
  const c = checks.value[name]
  if (!c) {
    return { tone: 'idle', label: '未测试', latency: 0, detail: '' }
  }
  const v = STATUS_VIEW[c.status] ?? STATUS_VIEW.error
  return { tone: v.tone, label: v.label, latency: c.latency_ms, detail: c.detail }
}

function applyResults(list: ProviderCheck[]) {
  const next = { ...checks.value }
  for (const r of list) next[r.provider] = r
  checks.value = next
}

/** 手动测试单个端点。 */
async function checkOne(name: string) {
  if (checking.value[name] || checkingAll.value) return
  checking.value = { ...checking.value, [name]: true }
  try {
    applyResults(await checkProviders(name))
  } catch {
    // 请求本身失败时保留上一次结果（页面顶部已有后端离线提示）
  } finally {
    checking.value = { ...checking.value, [name]: false }
  }
}

/** 并发测试全部端点（页面加载与「全部测试」按钮共用）。 */
async function checkAll() {
  if (checkingAll.value || !props.providers.length) return
  checkingAll.value = true
  const busy: Record<string, boolean> = {}
  props.providers.forEach((p) => {
    busy[p.name] = true
  })
  checking.value = busy
  try {
    applyResults(await checkProviders())
  } catch {
    // 同上：静默降级
  } finally {
    checking.value = {}
    checkingAll.value = false
  }
}

/** 卡片副标题：连通 x / 共 y。 */
const checkSummary = computed(() => {
  const total = props.providers.length
  if (!total) return '未配置端点'
  const done = props.providers.filter((p) => checks.value[p.name]).length
  if (!done) return '正在测试连通性…'
  const ok = props.providers.filter((p) => checks.value[p.name]?.status === 'ok').length
  return `连通 ${ok} / ${total}${done < total ? ' · 测试中' : ''}`
})

// 页面加载时测试一次连通性
onMounted(checkAll)
</script>

<template>
  <section class="page">
    <header class="page-head">
      <div>
        <h1 class="page-title">概览</h1>
        <p class="page-sub">{{ stepsHint }}</p>
      </div>
      <div class="head-actions">
        <div class="segmented">
          <button
            v-for="r in RANGES"
            :key="r.value"
            :class="{ active: days === r.value }"
            @click="emit('reload', r.value)"
          >
            {{ r.label }}
          </button>
        </div>
        <button class="btn ghost" @click="emit('reload', days)">
          <AppIcon name="refresh" :size="15" />
          刷新
        </button>
      </div>
    </header>

    <p v-if="stats === null" class="notice">
      <AppIcon name="db" :size="15" />
      调用统计已关闭（config.yml 的 storage.enabled=false），暂无指标数据。
    </p>

    <div class="grid-metrics card">
      <MetricCard
        label="总调用次数"
        :value="thousand(stats?.total_calls ?? 0)"
        :delta="deltaOf(callsSeries)"
        :hint="`今日 ${thousand(stats?.today_calls ?? 0)} 次`"
        :series="callsSeries"
      />
      <MetricCard
        label="Token 消耗"
        :value="compact(stats?.total_tokens ?? 0)"
        :delta="deltaOf(tokensSeries)"
        hint="提示 + 补全"
        :series="tokensSeries"
      />
      <MetricCard
        label="平均耗时"
        :value="duration(stats?.avg_latency_ms ?? 0)"
        :hint="`共 ${recent.length} 次采样`"
        :series="latencySeries"
      />
      <MetricCard
        label="调用成功率"
        :value="successRateText"
        :hint="`失败 ${thousand(
          daily.reduce((s, d) => s + d.failures, 0),
        )} 次`"
        :series="successSeries"
      />
    </div>

    <article class="card trend-card">
      <header class="card-head">
        <div>
          <p class="card-title">调用趋势</p>
          <p class="card-sub">最近 {{ days }} 天 · {{ metric === 'calls' ? '调用次数' : 'Token 消耗' }}</p>
        </div>
        <div class="segmented">
          <button
            v-for="m in METRICS"
            :key="m.key"
            :class="{ active: metric === m.key }"
            @click="metric = m.key"
          >
            {{ m.label }}
          </button>
        </div>
      </header>

      <div class="trend-total">
        <span class="trend-value">{{ thousand(trendTotal) }}</span>
        <span class="trend-unit">{{ metric === 'calls' ? '次调用' : 'tokens' }}</span>
      </div>

      <TrendChart :data="trendData" :color="trendColor" :height="196" />
    </article>

    <div class="grid-two">
      <article class="card">
        <header class="card-head">
          <p class="card-title">模型用量</p>
          <p class="card-sub">按调用次数排序</p>
        </header>
        <div class="table-wrap">
          <table v-if="modelRows.length" class="table">
            <thead>
              <tr>
                <th>模型</th>
                <th class="num">调用</th>
                <th class="num">Token</th>
                <th class="num">平均耗时</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="m in modelRows" :key="`${m.provider}|${m.model}`">
                <td>
                  <span class="cell-model num">{{ m.model }}</span>
                  <span class="cell-provider">{{ m.provider }}</span>
                </td>
                <td class="num">{{ thousand(m.calls) }}</td>
                <td class="num">{{ compact(m.tokens) }}</td>
                <td class="num">{{ duration(m.avg_latency_ms) }}</td>
              </tr>
            </tbody>
          </table>
          <p v-else class="blank">暂无模型调用数据</p>
        </div>
      </article>

      <article class="card">
        <header class="card-head">
          <div>
            <p class="card-title">AI 端点</p>
            <p class="card-sub">{{ providers.length }} 个 provider · {{ checkSummary }}</p>
          </div>
          <button
            class="btn ghost small"
            :disabled="checkingAll || !providers.length"
            @click="checkAll"
          >
            <AppIcon name="refresh" :size="14" :class="{ spin: checkingAll }" />
            {{ checkingAll ? '测试中…' : '全部测试' }}
          </button>
        </header>
        <ul class="providers">
          <li v-for="p in providers" :key="p.name" class="provider">
            <div class="provider-main">
              <span class="provider-name num">
                {{ p.name }}
                <span v-if="p.default" class="tag">默认</span>
              </span>
              <span class="provider-url">{{ p.base_url }}</span>
            </div>
            <div class="provider-right">
              <span class="tag">{{ p.type }}</span>
              <span class="state num" :class="statusOf(p.name).tone" :title="statusOf(p.name).detail">
                {{ statusOf(p.name).label }}
                <em v-if="statusOf(p.name).latency">· {{ statusOf(p.name).latency }}ms</em>
              </span>
              <button
                class="btn ghost tiny"
                :disabled="checking[p.name] || checkingAll"
                @click="checkOne(p.name)"
              >
                {{ checking[p.name] ? '…' : '测试' }}
              </button>
            </div>
          </li>
          <p v-if="!providers.length" class="blank">未配置 provider</p>
        </ul>
      </article>
    </div>

    <article class="card">
      <header class="card-head">
        <p class="card-title">最近调用</p>
        <p class="card-sub">最新 {{ recent.length }} 条</p>
      </header>
      <div class="table-wrap">
        <table v-if="recent.length" class="table">
          <thead>
            <tr>
              <th>时间</th>
              <th>模型</th>
              <th class="num">Token</th>
              <th class="num">耗时</th>
              <th>状态</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(row, i) in recent" :key="i">
              <td class="mono">{{ row.ts.slice(5) }}</td>
              <td>
                <span class="cell-model">{{ row.model }}</span>
                <span class="cell-provider">{{ row.provider }}</span>
              </td>
              <td class="num">{{ thousand(row.prompt_tokens + row.completion_tokens) }}</td>
              <td class="num">{{ duration(row.latency_ms) }}</td>
              <td>
                <span class="state num" :class="row.ok ? 'ok' : 'fail'">
                  {{ row.ok ? 'ok' : 'failed' }}
                </span>
              </td>
            </tr>
          </tbody>
        </table>
        <p v-else class="blank">暂无调用记录：到左侧子功能跑一次工资单审读试试。</p>
      </div>
    </article>

    <p v-if="!hasData && stats" class="foot-hint">
      提示：指标数据来自本地 SQLite（{{ app.name }}），仅统计启用 storage 后的调用流水。
    </p>
  </section>
</template>

<style scoped>
.page {
  display: flex;
  flex-direction: column;
  gap: 14px;
  max-width: 1240px;
}

.page-head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 18px;
  flex-wrap: wrap;
  margin-bottom: 4px;
}

.page-title {
  font-size: 25px;
  letter-spacing: -0.03em;
}

.page-sub {
  margin-top: 3px;
  color: var(--text-2);
  font-size: 13px;
}

.head-actions {
  display: flex;
  align-items: center;
  gap: 10px;
}

.notice {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 12px 15px;
  border-radius: var(--radius);
  background: var(--orange-soft);
  color: var(--warn-strong);
  font-size: 13px;
  font-weight: 600;
}

.grid-metrics {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
}

/* 四张指标卡共用一张表的 1px 分隔线，而不是四张独立阴影卡 */
.grid-metrics :deep(.metric) {
  border-left: 1px solid var(--border);
}

.grid-metrics :deep(.metric:first-child) {
  border-left: none;
}

.trend-card {
  padding-bottom: 4px;
}

.trend-total {
  display: flex;
  align-items: baseline;
  gap: 7px;
  padding: 14px 20px 2px;
}

.trend-value {
  font-size: 34px;
  font-weight: 700;
  letter-spacing: -0.035em;
  line-height: 1.05;
}

.trend-unit {
  color: var(--text-2);
  font-size: 13px;
  font-weight: 600;
}

.grid-two {
  display: grid;
  grid-template-columns: 1.3fr 1fr;
  gap: 14px;
}

/* ---- provider 列表 ---- */
.providers {
  list-style: none;
  margin: 0;
  padding: 6px 20px 16px;
  display: flex;
  flex-direction: column;
}

.provider {
  display: flex;
  align-items: center;
  gap: 11px;
  padding: 9px 0;
  border-top: 1px solid var(--border);
}

.provider:first-child {
  border-top: none;
}

.provider-right {
  display: flex;
  align-items: center;
  gap: 8px;
  flex: none;
}

.state {
  color: var(--text-3);
  font-size: 12px;
  white-space: nowrap;
}

.state em {
  font-style: normal;
  opacity: 0.75;
}

.state.ok {
  color: var(--ok);
}
.state.warn {
  color: var(--warn);
}
.state.off {
  color: var(--err);
}
.state.idle {
  color: var(--text-3);
}

.btn.small {
  padding: 6px 12px;
  font-size: 12.5px;
}

.btn.tiny {
  padding: 5px 11px;
  font-size: 12px;
  border-radius: 10px;
}

.spin {
  animation: spin 1s linear infinite;
}

@keyframes spin {
  to {
    transform: rotate(360deg);
  }
}

.provider-main {
  display: flex;
  flex-direction: column;
  min-width: 0;
  flex: 1;
}

.provider-name {
  display: flex;
  align-items: center;
  gap: 7px;
  font-size: 13.5px;
  font-weight: 650;
}

.provider-url {
  color: var(--text-3);
  font-size: 11.5px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* ---- 表格 ---- */
.table-wrap {
  padding: 12px 20px 20px;
}

.table {
  width: 100%;
  border-collapse: collapse;
}

.table th {
  padding: 6px 10px 9px;
  color: var(--text-3);
  font-size: 11.5px;
  font-weight: 700;
  text-align: left;
  letter-spacing: 0.02em;
}

.table td {
  padding: 10px;
  border-top: 1px solid var(--border);
  font-size: 13px;
}

.table .num {
  text-align: right;
  font-variant-numeric: tabular-nums;
}

.mono {
  color: var(--text-2);
  font-variant-numeric: tabular-nums;
  font-size: 12.5px;
}

.cell-model {
  font-weight: 600;
}

.cell-provider {
  margin-left: 8px;
  color: var(--text-3);
  font-size: 11.5px;
}

/* 最近调用的状态列：mono 小写文字 + 语义色 */
.state.num {
  font-size: 12px;
}

.state.num.ok {
  color: var(--ok);
  background: none;
}

.state.num.fail {
  color: var(--err);
  background: none;
}

.blank {
  padding: 16px 0;
  color: var(--text-3);
  font-size: 13px;
}

.foot-hint {
  color: var(--text-3);
  font-size: 12px;
  padding: 2px 4px;
}

@media (max-width: 1180px) {
  .grid-metrics {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
  .grid-two {
    grid-template-columns: 1fr;
  }
}
</style>
