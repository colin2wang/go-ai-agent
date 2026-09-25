<script setup lang="ts">
import { computed } from 'vue'
import { areaPath, smoothLine, scale, type Pt } from '../chart'

const props = withDefaults(
  defineProps<{
    data: { label: string; value: number }[]
    color?: string
    height?: number
  }>(),
  { color: 'var(--blue)', height: 200 },
)

let seq = 0
const gid = `trend-${Date.now().toString(36)}-${(seq += 1)}`

const PLOT_TOP = 12
const PLOT_BOTTOM = 88
const GRID = [PLOT_TOP, (PLOT_TOP + PLOT_BOTTOM) / 2, PLOT_BOTTOM]

const values = computed(() => props.data.map((d) => d.value))
const points = computed<Pt[]>(() => scale(values.value, 0, 100, PLOT_TOP, PLOT_BOTTOM))
const line = computed(() => smoothLine(points.value, 0.16))
const area = computed(() => areaPath(line.value, points.value, PLOT_BOTTOM))

/** 峰值点高亮（对应截图里的实心圆点）。 */
const peak = computed(() => {
  const pts = points.value
  if (pts.length < 2) return null
  let idx = 0
  values.value.forEach((v, i) => {
    if (v > values.value[idx]) idx = i
  })
  return pts[idx]
})

const ticks = computed(() => {
  const len = props.data.length
  if (len === 0) return [] as { key: number; x: number; label: string }[]
  const step = Math.max(1, Math.ceil(len / 7))
  const out: { key: number; x: number; label: string }[] = []
  for (let i = 0; i < len; i += step) {
    out.push({ key: i, x: points.value[i]?.x ?? 0, label: props.data[i].label })
  }
  const last = len - 1
  if (out[out.length - 1]?.key !== last) {
    out.push({ key: last, x: points.value[last]?.x ?? 0, label: props.data[last].label })
  }
  return out
})

const hasData = computed(() => props.data.length > 0 && values.value.some((v) => v > 0))
</script>

<template>
  <div class="trend-wrap">
    <div class="trend" :style="{ height: `${height}px` }">
      <svg class="canvas" viewBox="0 0 100 100" preserveAspectRatio="none">
        <defs>
          <linearGradient :id="gid" x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" :stop-color="color" stop-opacity="0.22" />
            <stop offset="100%" :stop-color="color" stop-opacity="0" />
          </linearGradient>
        </defs>
        <line
          v-for="(y, i) in GRID"
          :key="i"
          x1="0"
          :y1="y"
          x2="100"
          :y2="y"
          stroke="#e6e6ec"
          stroke-width="1"
          stroke-dasharray="3 5"
          vector-effect="non-scaling-stroke"
        />
        <path v-if="area" :d="area" :fill="`url(#${gid})`" />
        <path
          v-if="line"
          :d="line"
          fill="none"
          :stroke="color"
          stroke-width="2.2"
          stroke-linecap="round"
          stroke-linejoin="round"
          vector-effect="non-scaling-stroke"
        />
      </svg>
      <span
        v-if="peak && hasData"
        class="peak"
        :style="{ left: `${peak.x}%`, top: `${peak.y}%`, background: color }"
      />
      <div v-if="!hasData" class="empty">该区间暂无调用记录</div>
    </div>

    <div class="ticks">
      <span v-for="t in ticks" :key="t.key" :style="{ left: `${t.x}%` }">{{ t.label }}</span>
    </div>
  </div>
</template>

<style scoped>
.trend-wrap {
  padding: 4px 20px 16px;
}

.trend {
  position: relative;
  width: 100%;
}

.canvas {
  display: block;
  width: 100%;
  height: 100%;
  overflow: visible;
}

.peak {
  position: absolute;
  width: 9px;
  height: 9px;
  border-radius: 50%;
  transform: translate(-50%, -50%);
  box-shadow: 0 0 0 3px #fff;
}

.empty {
  position: absolute;
  inset: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  color: var(--text-3);
  font-size: 13px;
}

.ticks {
  position: relative;
  height: 18px;
  margin-top: 8px;
}

.ticks span {
  position: absolute;
  top: 0;
  transform: translateX(-50%);
  color: var(--text-3);
  font-size: 11px;
  white-space: nowrap;
}
</style>
