<script setup lang="ts">
import { nextTick, ref, watch } from 'vue'
import { clearLog, logLines } from '../logstore'

const collapsed = ref(false)
const bodyEl = ref<HTMLElement | null>(null)

// 新日志到达且面板展开时滚动到底部（最近事件最醒目）
watch(
  () => logLines.value.length,
  () => {
    if (collapsed.value) return
    void nextTick(() => {
      if (bodyEl.value) bodyEl.value.scrollTop = bodyEl.value.scrollHeight
    })
  },
)
</script>

<template>
  <section class="logpanel">
    <header class="log-head">
      <button class="log-toggle" @click="collapsed = !collapsed">
        <span class="log-caret">{{ collapsed ? '▸' : '▾' }}</span>
        运行日志
        <span class="log-count num">{{ logLines.length }}</span>
      </button>
      <button v-if="!collapsed" class="log-clear" @click="clearLog">清空</button>
    </header>
    <div v-if="!collapsed" ref="bodyEl" class="log-body">
      <p v-for="(l, i) in logLines" :key="i" class="log-line" :class="l.level">
        <span class="log-ts num">{{ l.ts }}</span>
        <span class="log-lv">{{ l.level.toUpperCase() }}</span>
        <span class="log-msg">{{ l.msg }}</span>
      </p>
      <p v-if="!logLines.length" class="log-empty">暂无日志</p>
    </div>
  </section>
</template>

<style scoped>
.logpanel {
  flex: none;
  border-top: 1px solid var(--border);
  background: var(--surface);
  display: flex;
  flex-direction: column;
}

.log-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 6px 14px;
}

.log-toggle {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  color: var(--text-2);
  font-size: 12px;
  font-weight: 600;
}

.log-toggle:hover {
  color: var(--text);
}

.log-caret {
  color: var(--text-3);
  font-size: 10px;
}

.log-count {
  padding: 0 6px;
  border-radius: 999px;
  background: var(--surface-2);
  border: 1px solid var(--border);
  color: var(--text-3);
  font-size: 10.5px;
}

.log-clear {
  color: var(--text-3);
  font-size: 12px;
}

.log-clear:hover {
  color: var(--err);
}

.log-body {
  height: 170px;
  overflow-y: auto;
  padding: 2px 14px 10px;
  font-family: var(--font-mono);
  font-size: 12px;
  line-height: 1.7;
}

.log-line {
  display: flex;
  gap: 10px;
  white-space: pre-wrap;
  word-break: break-word;
}

.log-ts {
  flex: none;
  color: var(--text-3);
  font-size: 11px;
}

.log-lv {
  flex: none;
  width: 42px;
  color: var(--text-3);
  font-size: 11px;
}

.log-line.warn .log-lv {
  color: var(--warn);
}

.log-line.error .log-lv {
  color: var(--err);
}

.log-line.error .log-msg {
  color: var(--err);
}

.log-msg {
  color: var(--text-2);
  min-width: 0;
}

.log-empty {
  color: var(--text-3);
}
</style>
