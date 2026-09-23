<script setup lang="ts">
import { ref, computed } from 'vue'
import { reviewPayslip, health } from './api'
import type { JobResult } from './types'

const file = ref<File | null>(null)
const previewUrl = ref<string>('')
const loading = ref(false)
const error = ref('')
const result = ref<JobResult | null>(null)
const backendOk = ref<boolean | null>(null)

const activeTab = ref<'report' | 'structured' | 'ocr' | 'translation'>('report')

function onFileChange(e: Event) {
  const input = e.target as HTMLInputElement
  const f = input.files?.[0] ?? null
  file.value = f
  result.value = null
  error.value = ''
  if (previewUrl.value) URL.revokeObjectURL(previewUrl.value)
  previewUrl.value = f ? URL.createObjectURL(f) : ''
}

async function submit() {
  if (!file.value || loading.value) return
  loading.value = true
  error.value = ''
  result.value = null
  try {
    result.value = await reviewPayslip(file.value)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}

async function checkHealth() {
  backendOk.value = await health()
}

const structuredRows = computed(() => {
  const s = result.value?.structured
  if (!s) return []
  return Object.entries(s).map(([k, v]) => ({ key: k, value: JSON.stringify(v) }))
})
</script>

<template>
  <main class="container">
    <h1>工资单 AI 审读</h1>
    <p class="subtitle">
      上传工资单截图 → OCR 提取 → 翻译 → 审读校验 → 专家分析 → 汇总报告
      <button class="link" @click="checkHealth">
        {{ backendOk === null ? '检测后端' : backendOk ? '● 后端在线' : '● 后端离线' }}
      </button>
    </p>

    <section class="upload">
      <input id="file-input" type="file" accept="image/png,image/jpeg" @change="onFileChange" />
      <button :disabled="!file || loading" @click="submit">
        {{ loading ? '审读中…（本地模型可能需要 1-5 分钟）' : '开始审读' }}
      </button>
    </section>

    <p v-if="error" class="error">✗ {{ error }}</p>

    <section v-if="previewUrl" class="preview">
      <h2>原始图片</h2>
      <img :src="previewUrl" alt="工资单预览" />
    </section>

    <section v-if="result" class="result">
      <p class="meta">
        任务 {{ result.job_id }} · 状态
        <b :class="result.status">{{ result.status }}</b>
        · 耗时 {{ (result.duration_ms / 1000).toFixed(1) }}s
      </p>
      <p v-if="result.error" class="error">✗ {{ result.error }}</p>

      <nav v-if="result.status === 'done'" class="tabs">
        <button :class="{ active: activeTab === 'report' }" @click="activeTab = 'report'">最终报告</button>
        <button :class="{ active: activeTab === 'structured' }" @click="activeTab = 'structured'">结构化数据</button>
        <button :class="{ active: activeTab === 'ocr' }" @click="activeTab = 'ocr'">OCR 结果</button>
        <button :class="{ active: activeTab === 'translation' }" @click="activeTab = 'translation'">翻译</button>
      </nav>

      <div v-if="activeTab === 'report'" class="report">{{ result.final_report }}</div>

      <table v-if="activeTab === 'structured' && structuredRows.length">
        <thead><tr><th>字段</th><th>值</th></tr></thead>
        <tbody>
          <tr v-for="row in structuredRows" :key="row.key">
            <td>{{ row.key }}</td><td>{{ row.value }}</td>
          </tr>
        </tbody>
      </table>

      <pre v-if="activeTab === 'ocr'" class="raw">{{ result.ocr_text }}</pre>
      <pre v-if="activeTab === 'translation'" class="raw">{{ result.translation }}</pre>

      <div v-if="result.review_notes?.length" class="notes">
        <h3>审读发现的问题</h3>
        <ul><li v-for="(n, i) in result.review_notes" :key="i">{{ n }}</li></ul>
      </div>
    </section>
  </main>
</template>

<style>
:root { color-scheme: light dark; }
body { margin: 0; font-family: system-ui, 'Segoe UI', 'Microsoft YaHei', sans-serif; }
.container { max-width: 880px; margin: 0 auto; padding: 24px 16px 64px; }
h1 { font-size: 1.5rem; }
.subtitle { color: #888; font-size: 0.9rem; }
.link { border: 0; background: none; color: #4a90d9; cursor: pointer; font-size: inherit; }
.upload { display: flex; gap: 12px; align-items: center; margin: 16px 0; }
button { padding: 8px 18px; border: 1px solid #4a90d9; background: #4a90d9; color: #fff; border-radius: 6px; cursor: pointer; }
button:disabled { opacity: 0.5; cursor: not-allowed; }
.error { color: #d9534f; }
.preview img { max-width: 100%; max-height: 420px; border: 1px solid #ccc; border-radius: 8px; }
.meta { color: #666; font-size: 0.9rem; }
.meta b.done { color: #2e7d32; }
.meta b.failed { color: #d9534f; }
.tabs { display: flex; gap: 8px; margin: 12px 0; }
.tabs button { background: none; color: inherit; border: 1px solid #999; }
.tabs button.active { background: #4a90d9; color: #fff; border-color: #4a90d9; }
.report { white-space: pre-wrap; line-height: 1.7; padding: 12px; border: 1px solid #ddd; border-radius: 8px; }
.raw { white-space: pre-wrap; padding: 12px; background: rgba(127,127,127,0.08); border-radius: 8px; max-height: 420px; overflow: auto; }
table { border-collapse: collapse; width: 100%; }
th, td { border: 1px solid #ddd; padding: 6px 10px; text-align: left; }
.notes h3 { font-size: 1rem; }
</style>
