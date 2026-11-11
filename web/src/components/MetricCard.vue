<script setup lang="ts">
import { computed } from 'vue'
import Sparkline from './Sparkline.vue'

const props = withDefaults(
  defineProps<{
    label: string
    value: string
    unit?: string
    /** 环比变化百分比，正负决定配色与箭头。 */
    delta?: number
    /** delta 缺失时展示的补充说明。 */
    hint?: string
    /** 已废弃（视觉去彩后不再区分色调），仅为兼容旧调用保留。 */
    tone?: string
    icon?: string
    series?: number[]
  }>(),
  { series: () => [] },
)

const hasSeries = computed(() => props.series.length > 1)
</script>

<template>
  <article class="metric">
    <span class="label">{{ label }}</span>

    <div class="metric-row">
      <div class="value metric-value">
        {{ value }}<span v-if="unit" class="unit">{{ unit }}</span>
      </div>
      <span v-if="delta !== undefined" class="delta num" :class="delta >= 0 ? 'up' : 'down'">
        {{ delta >= 0 ? '↑' : '↓' }} {{ Math.abs(delta).toFixed(1) }}%
      </span>
    </div>
    <span v-if="delta === undefined && hint" class="hint">{{ hint }}</span>

    <Sparkline v-if="hasSeries" :data="series" color="var(--text-2)" :height="32" />
  </article>
</template>

<style scoped>
.metric {
  padding: 14px 16px 12px;
  display: flex;
  flex-direction: column;
  gap: 8px;
  overflow: hidden;
}

.label {
  color: var(--text-2);
  font-size: 12.5px;
}

.metric-row {
  display: flex;
  align-items: baseline;
  gap: 8px;
  min-height: 30px;
}

.value {
  font-size: 26px;
  line-height: 1.1;
}

.unit {
  margin-left: 3px;
  font-size: 13px;
  color: var(--text-2);
}

.delta {
  font-size: 11.5px;
  font-weight: 600;
}

.delta.up {
  color: var(--ok);
}

.delta.down {
  color: var(--err);
}

.hint {
  color: var(--text-3);
  font-size: 12px;
}
</style>
