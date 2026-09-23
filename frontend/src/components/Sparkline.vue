<script setup lang="ts">
import { computed } from 'vue'
import { areaPath, smoothLine, scale } from '../chart'

const props = withDefaults(
  defineProps<{ data: number[]; color: string; height?: number }>(),
  { height: 40 },
)

let seq = 0
const gid = `spark-${Date.now().toString(36)}-${(seq += 1)}`

const W = 240
const PAD = 5

const points = computed(() =>
  scale(props.data, 0, W, PAD, props.height - PAD),
)
const line = computed(() => smoothLine(points.value))
const area = computed(() => areaPath(line.value, points.value, props.height))
</script>

<template>
  <svg
    class="spark"
    :viewBox="`0 0 ${W} ${height}`"
    :height="height"
    width="100%"
    preserveAspectRatio="none"
  >
    <defs>
      <linearGradient :id="gid" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0%" :stop-color="color" stop-opacity="0.26" />
        <stop offset="100%" :stop-color="color" stop-opacity="0" />
      </linearGradient>
    </defs>
    <path v-if="area" :d="area" :fill="`url(#${gid})`" />
    <path
      v-if="line"
      :d="line"
      fill="none"
      :stroke="color"
      stroke-width="2"
      stroke-linecap="round"
      stroke-linejoin="round"
      vector-effect="non-scaling-stroke"
    />
  </svg>
</template>

<style scoped>
.spark {
  display: block;
}
</style>
