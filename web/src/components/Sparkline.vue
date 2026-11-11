<script setup lang="ts">
import { computed } from 'vue'
import { smoothLine, scale } from '../chart'

const props = withDefaults(
  defineProps<{ data: number[]; color: string; height?: number }>(),
  { height: 32 },
)

const W = 240
const PAD = 3

const points = computed(() =>
  scale(props.data, 0, W, PAD, props.height - PAD),
)
const line = computed(() => smoothLine(points.value))
</script>

<template>
  <svg
    class="spark"
    :viewBox="`0 0 ${W} ${height}`"
    :height="height"
    width="100%"
    preserveAspectRatio="none"
  >
    <path
      v-if="line"
      :d="line"
      fill="none"
      :stroke="color"
      stroke-width="1.5"
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
