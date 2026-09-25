<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import Sidebar from './components/Sidebar.vue'
import OverviewView from './views/OverviewView.vue'
import FeatureView from './views/FeatureView.vue'
import ChatView from './views/ChatView.vue'
import { fetchFeatures, fetchOverview, health } from './api'
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
  backendOk.value = await health()
  if (backendOk.value) {
    try {
      const res = await fetchFeatures()
      features.value = res.features
      appInfo.value = res.app
      providers.value = res.providers
    } catch {
      // 忽略：菜单为空时仍可展示概览
    }
    await loadOverview()
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
</template>

<style scoped>
.shell {
  display: flex;
  align-items: stretch;
  min-height: 100vh;
}

.content {
  flex: 1;
  min-width: 0;
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
