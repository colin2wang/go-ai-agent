<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import Sidebar from './components/Sidebar.vue'
import LogPanel from './components/LogPanel.vue'
import OverviewView from './views/OverviewView.vue'
import FeatureView from './views/FeatureView.vue'
import ChatView from './views/ChatView.vue'
import { fetchFeatures, fetchOverview, health, waitForBackend } from './api'
import { pushLog } from './logstore'
import type { AppInfo, FeatureInfo, OverviewResponse, ProviderInfo } from './types'

const appInfo = ref<AppInfo>({ name: 'go-ai-agent', title: 'AI 工作流平台', version: '' })
const features = ref<FeatureInfo[]>([])
const providers = ref<ProviderInfo[]>([])
const overview = ref<OverviewResponse | null>(null)

const backendOk = ref<boolean | null>(null)
const booting = ref(true)
const days = ref(14)
const activeId = ref('overview')

const activeFeature = computed(
  () => features.value.find((f) => f.id === activeId.value) ?? null,
)

async function loadOverview() {
  try {
    const res = await fetchOverview(days.value)
    overview.value = res
    appInfo.value = res.app
    if (res.providers.length) providers.value = res.providers
  } catch {
    // 概览不可用时静默降级，不影响子功能页面
  }
}

async function boot() {
  booting.value = true
  // 桌面模式：先等 Wails 绑定注入完成，再短暂重试（SQLite 初始化期间调用也可能失败）
  await waitForBackend()
  for (let i = 0; i < 5 && !(backendOk.value = await health()); i++) {
    await new Promise((r) => setTimeout(r, 400))
  }
  if (backendOk.value) {
    pushLog('info', `后端已连接（${appInfo.value.name} v${appInfo.value.version || 'dev'}）`)
    try {
      const res = await fetchFeatures()
      features.value = res.features
      appInfo.value = res.app
      providers.value = res.providers
      pushLog('info', `已加载 ${features.value.length} 个子功能、${providers.value.length} 个端点`)
    } catch {
      pushLog('warn', '子功能清单加载失败，菜单可能为空')
    }
    await loadOverview()
  } else {
    pushLog('error', '后端不可用：请确认 harness 已启动（默认读取 config/config.yml）')
  }
  booting.value = false
}

function onReload(nextDays: number) {
  days.value = nextDays
  void loadOverview()
}

onMounted(boot)
</script>

<template>
  <div class="term-window">
    <div class="term-titlebar">
      <div class="term-lights"><span class="l-r" /><span class="l-y" /><span class="l-g" /></div>
      <div class="term-title">~/{{ appInfo.name }} — harness — zsh</div>
      <div class="term-titlebar-spacer" />
    </div>

    <div class="shell">
    <Sidebar
      :app="appInfo"
      :features="features"
      :active="activeId"
      :health="backendOk"
      @select="activeId = $event"
    />

    <main class="content">
      <div v-if="booting" class="boot">正在连接后端…</div>

      <template v-else-if="backendOk === false">
        <div class="boot">
          <p class="boot-title">后端不可用</p>
          <p class="boot-sub">
            请确认 harness 已启动（默认读取 config/config.yml），然后刷新页面。
          </p>
        </div>
      </template>

      <template v-else>
        <OverviewView
          v-if="activeId === 'overview'"
          :overview="overview"
          :app="appInfo"
          :providers="providers"
          :days="days"
          @reload="onReload"
        />
        <ChatView
          v-else-if="activeFeature?.kind === 'chat'"
          :key="activeFeature.id"
          :feature="activeFeature"
          :providers="providers"
          @done="loadOverview"
        />
        <FeatureView
          v-else-if="activeFeature"
          :key="activeFeature.id"
          :feature="activeFeature"
          @done="loadOverview"
        />
        <div v-else class="boot">子功能不存在或未启用</div>
      </template>
    </main>
    </div>

    <LogPanel />
  </div>
</template>

<style scoped>
/* 应用壳：固定一屏，内容区内部滚动，底部日志面板常驻 */
.term-window {
  display: flex;
  flex-direction: column;
  height: 100vh;
}

.shell {
  display: flex;
  align-items: stretch;
  flex: 1;
  min-height: 0;
}

.content {
  flex: 1;
  min-width: 0;
  overflow-y: auto;
  padding: 26px 30px 64px;
}

.boot {
  padding: 48px 4px;
  color: var(--text-2);
  font-size: 13.5px;
}

.boot-title {
  font-size: 16px;
  font-weight: 700;
  color: var(--text);
  margin-bottom: 5px;
}

.boot-sub {
  color: var(--text-2);
}
</style>
