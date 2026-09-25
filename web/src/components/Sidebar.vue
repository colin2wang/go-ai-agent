<script setup lang="ts">
import AppIcon from './AppIcon.vue'
import type { AppInfo, FeatureInfo } from '../types'

defineProps<{
  app: AppInfo
  features: FeatureInfo[]
  active: string
  health: boolean | null
}>()

defineEmits<{ (e: 'select', id: string): void }>()
</script>

<template>
  <aside class="sidebar">
    <div class="brand">
      <span class="brand-mark"><AppIcon name="spark" :size="17" /></span>
      <span class="brand-text">
        <span class="brand-title">{{ app.title || 'AI 工作流平台' }}</span>
        <span class="brand-sub">{{ app.name || 'go-ai-agent' }}</span>
      </span>
    </div>

    <nav class="nav">
      <p class="nav-group">主菜单</p>
      <button
        class="nav-item"
        :class="{ active: active === 'overview' }"
        @click="$emit('select', 'overview')"
      >
        <AppIcon name="overview" :size="17" />
        <span>概览</span>
      </button>

      <p class="nav-group">子功能</p>
      <button
        v-for="f in features"
        :key="f.id"
        class="nav-item"
        :class="{ active: active === f.id }"
        @click="$emit('select', f.id)"
      >
        <AppIcon :name="f.icon || 'receipt'" :size="17" />
        <span class="nav-label">{{ f.name }}</span>
      </button>
      <p v-if="!features.length" class="nav-empty">暂无启用的子功能</p>
    </nav>

    <div class="status">
      <span class="dot" :class="health === null ? 'wait' : health ? 'on' : 'off'" />
      <span>{{ health === null ? '检测后端…' : health ? '后端在线' : '后端离线' }}</span>
    </div>
  </aside>
</template>

<style scoped>
.sidebar {
  width: var(--sidebar-w);
  flex: none;
  display: flex;
  flex-direction: column;
  gap: 22px;
  padding: 22px 16px 18px;
  background: rgba(255, 255, 255, 0.82);
  border-right: 1px solid var(--border);
  backdrop-filter: saturate(180%) blur(18px);
  position: sticky;
  top: 0;
  height: 100vh;
}

.brand {
  display: flex;
  align-items: center;
  gap: 11px;
  padding: 0 6px;
}

.brand-mark {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 34px;
  height: 34px;
  border-radius: 11px;
  background: #1d1d1f;
  color: #fff;
  flex: none;
}

.brand-text {
  display: flex;
  flex-direction: column;
  min-width: 0;
}

.brand-title {
  font-size: 14px;
  font-weight: 700;
  letter-spacing: -0.01em;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.brand-sub {
  color: var(--text-3);
  font-size: 11.5px;
}

.nav {
  display: flex;
  flex-direction: column;
  gap: 3px;
  overflow-y: auto;
  flex: 1;
}

.nav-group {
  margin: 14px 8px 5px;
  color: var(--text-3);
  font-size: 11px;
  font-weight: 700;
  letter-spacing: 0.06em;
  text-transform: uppercase;
}

.nav-group:first-child {
  margin-top: 0;
}

.nav-item {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  padding: 9px 11px;
  border-radius: 12px;
  color: var(--text-2);
  font-size: 13.5px;
  font-weight: 600;
  text-align: left;
  transition: background 0.15s ease, color 0.15s ease;
}

.nav-item:hover {
  background: var(--surface-2);
  color: var(--text);
}

.nav-item.active {
  background: var(--surface);
  color: var(--text);
  box-shadow: var(--shadow);
}

.nav-item.active :deep(svg) {
  color: var(--blue);
}

.nav-label {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.nav-empty {
  padding: 6px 12px;
  color: var(--text-3);
  font-size: 12.5px;
}

.status {
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 11px 13px;
  border-radius: 14px;
  background: var(--surface);
  border: 1px solid var(--border);
  color: var(--text-2);
  font-size: 12.5px;
  font-weight: 600;
}
</style>
