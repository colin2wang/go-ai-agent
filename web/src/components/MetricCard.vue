<script setup lang="ts">
import { computed } from 'vue'
import AppIcon from './AppIcon.vue'
import Sparkline from './Sparkline.vue'

type Tone = 'green' | 'blue' | 'purple' | 'orange'

const props = withDefaults(
  defineProps<{
    label: string
    value: string
    unit?: string
    /** 环比变化百分比，正负决定配色与箭头。 */
    delta?: number
    /** delta 缺失时展示的补充说明。 */
    hint?: string
    tone?: Tone
    icon?: string
    series?: number[]
  }>(),
  { tone: 'blue', icon: 'bolt', series: () => [] },
)

const TONE: Record<Tone, { fg: string; bg: string }> = {
  green: { fg: 'var(--green)', bg: 'var(--green-soft)' },
  blue: { fg: 'var(--blue)', bg: 'var(--blue-soft)' },
  purple: { fg: 'var(--purple)', bg: 'var(--purple-soft)' },
  orange: { fg: 'var(--orange)', bg: 'var(--orange-soft)' },
}

const tone = computed(() => TONE[props.tone])
const hasSeries = computed(() => props.series.length > 1)
</script>

<template>
  <article class="metric card">
    <header class="metric-top">
      <span class="chip" :style="{ color: tone.fg, background: tone.bg }">
        <AppIcon :name="icon" :size="14" />
      </span>
      <span class="label">{{ label }}</span>
    </header>

    <div class="metric-row">
      <div class="value">
        {{ value }}<span v-if="unit" class="unit">{{ unit }}</span>
      </div>
      <span v-if="delta !== undefined" class="delta" :class="delta >= 0 ? 'up' : 'down'">
        <svg width="9" height="9" viewBox="0 0 10 10" aria-hidden="true">
          <path
            :d="delta >= 0 ? 'M5 1.5 9 8H1z' : 'M5 8.5 1 2h8z'"
            fill="currentColor"
          />
        </svg>
        {{ Math.abs(delta).toFixed(0) }}%
      </span>
      <span v-else-if="hint" class="hint">{{ hint }}</span>
    </div>

    <Sparkline v-if="hasSeries" :data="series" :color="tone.fg" :height="38" />
  </article>
</template>

<style scoped>
.metric {
  padding: 16px 16px 12px;
  display: flex;
  flex-direction: column;
  gap: 10px;
  overflow: hidden;
}

.metric-top {
  display: flex;
  align-items: center;
  gap: 8px;
}

.chip {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 24px;
  height: 24px;
  border-radius: 8px;
  flex: none;
}

.label {
  color: var(--text-2);
  font-size: 12.5px;
  font-weight: 600;
}

.metric-row {
  display: flex;
  align-items: baseline;
  gap: 8px;
  min-height: 34px;
}

.value {
  font-size: 27px;
  font-weight: 700;
  letter-spacing: -0.025em;
  line-height: 1.1;
}

.unit {
  margin-left: 3px;
  font-size: 14px;
  font-weight: 600;
  color: var(--text-2);
}

.delta {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  padding: 2px 7px;
  border-radius: 999px;
  font-size: 11.5px;
  font-weight: 700;
}

.delta.up {
  color: var(--ok-strong);
  background: var(--green-soft);
}

.delta.down {
  color: var(--bad-strong);
  background: var(--red-soft);
}

.hint {
  color: var(--text-3);
  font-size: 12px;
  font-weight: 500;
}
</style>
