<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'
import AppIcon from '../components/AppIcon.vue'
import { runFeature } from '../api'
import { duration, thousand } from '../chart'
import type { FeatureInfo, JobResult, TOCEntry } from '../types'

const props = defineProps<{ feature: FeatureInfo }>()
const emit = defineEmits<{ (e: 'done'): void }>()

/** 节点名 → 展示标签与图标（未知节点兜底展示原名）。 */
const STEP_META: Record<string, { label: string; icon: string }> = {
  ocr: { label: 'OCR 提取', icon: 'scan' },
  merge: { label: '跨页合并', icon: 'layers' },
  render: { label: '模板渲染', icon: 'doc' },
  translate: { label: '翻译', icon: 'translate' },
  proofread: { label: '审读校验', icon: 'shield' },
  expert: { label: '专家分析', icon: 'spark' },
  analyze: { label: '汇总报告', icon: 'doc' },
}

const pipeline = computed(() =>
  props.feature.pipeline.map((name) => ({
    name,
    label: STEP_META[name]?.label ?? name,
    icon: STEP_META[name]?.icon ?? 'doc',
  })),
)

const multiple = computed(() => props.feature.input?.multiple ?? false)
const accept = computed(
  () => props.feature.input?.accept ?? 'image/png,image/jpeg,image/webp',
)
const hint = computed(() => props.feature.input?.hint ?? '本地模型处理通常需要 1–5 分钟')

interface Preview {
  name: string
  url: string
}

const files = ref<File[]>([])
const previews = ref<Preview[]>([])
const dragging = ref(false)
const loading = ref(false)
const error = ref('')
const result = ref<JobResult | null>(null)
const activeTab = ref('report')

function clearPreviews() {
  previews.value.forEach((p) => URL.revokeObjectURL(p.url))
  previews.value = []
}

function reset() {
  result.value = null
  error.value = ''
  activeTab.value = 'report'
}

function setFiles(list: File[]) {
  clearPreviews()
  files.value = list
  previews.value = list.map((f) => ({ name: f.name, url: URL.createObjectURL(f) }))
  reset()
}

function onFileChange(e: Event) {
  const input = e.target as HTMLInputElement
  const list = Array.from(input.files ?? [])
  setFiles(multiple.value ? list : list.slice(0, 1))
  input.value = ''
}

function onDrop(e: DragEvent) {
  dragging.value = false
  const list = Array.from(e.dataTransfer?.files ?? [])
  if (list.length) setFiles(multiple.value ? list : list.slice(0, 1))
}

function removeAt(index: number) {
  const next = files.value.filter((_, i) => i !== index)
  setFiles(next)
}

async function submit() {
  if (!files.value.length || loading.value) return
  loading.value = true
  reset()
  try {
    result.value = await runFeature(props.feature.id, files.value)
    emit('done')
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}

/** 图书目录条目（structured.entries）。 */
const entries = computed<TOCEntry[]>(() => {
  const raw = result.value?.structured?.entries
  return Array.isArray(raw) ? (raw as TOCEntry[]) : []
})

const tabs = computed(() => {
  const list: { key: string; label: string }[] = [{ key: 'report', label: '生成结果' }]
  if (entries.value.length) {
    list.push({ key: 'entries', label: `目录条目 ${entries.value.length}` })
  }
  list.push({ key: 'structured', label: '结构化数据' })
  list.push({ key: 'ocr', label: '原始 OCR' })
  if (result.value?.translation) list.push({ key: 'translation', label: '翻译' })
  return list
})

const structuredRows = computed(() => {
  const s = result.value?.structured
  if (!s) return []
  return Object.entries(s)
    .filter(([k]) => k !== 'entries') // 条目有专属表格
    .map(([k, v]) => ({ key: k, value: typeof v === 'string' ? v : JSON.stringify(v) }))
})

function download() {
  const text = result.value?.final_report ?? ''
  if (!text) return
  const ext = props.feature.output?.extension || '.md'
  const blob = new Blob([text], { type: 'text/plain;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = `${props.feature.id}-${Date.now()}${ext}`
  a.click()
  URL.revokeObjectURL(url)
}

onBeforeUnmount(clearPreviews)
</script>

<template>
  <section class="page">
    <header class="page-head">
      <div>
        <h1 class="page-title">{{ feature.name }}</h1>
        <p class="page-sub">{{ feature.description }}</p>
      </div>
    </header>

    <div class="pipeline card">
      <span class="pipeline-label">工作流</span>
      <template v-for="(s, i) in pipeline" :key="s.name">
        <span class="pipeline-node">
          <AppIcon :name="s.icon" :size="14" />
          {{ s.label }}
        </span>
        <span v-if="i < pipeline.length - 1" class="pipeline-arrow">→</span>
      </template>
      <span v-if="feature.output?.template" class="pipeline-tag">
        <AppIcon name="doc" :size="13" />
        模板 {{ feature.output.template }}
      </span>
    </div>

    <div class="grid">
      <article class="card panel">
        <header class="card-head">
          <p class="card-title">{{ multiple ? '上传页面' : '上传文件' }}</p>
          <p class="card-sub">{{ multiple ? `已选 ${files.length} 页` : '单文件' }}</p>
        </header>

        <label
          class="dropzone"
          :class="{ dragging, filled: files.length > 0 }"
          @dragover.prevent="dragging = true"
          @dragleave.prevent="dragging = false"
          @drop.prevent="onDrop"
        >
          <input
            type="file"
            :accept="accept"
            :multiple="multiple"
            @change="onFileChange"
          />
          <template v-if="!files.length">
            <span class="drop-icon"><AppIcon name="upload" :size="22" /></span>
            <span class="drop-title">
              {{ multiple ? '拖拽多张页面到此处，或点击选择' : '拖拽文件到此处，或点击选择' }}
            </span>
            <span class="drop-sub">{{ hint }}</span>
          </template>
          <div v-else class="thumbs" :class="{ multi: multiple }">
            <figure v-for="(p, i) in previews" :key="p.url" class="thumb">
              <img :src="p.url" :alt="p.name" />
              <figcaption>
                <span v-if="multiple" class="thumb-index">{{ i + 1 }}</span>
                <span class="thumb-name">{{ p.name }}</span>
              </figcaption>
              <button type="button" class="thumb-remove" title="移除" @click.prevent.stop="removeAt(i)">
                ×
              </button>
            </figure>
          </div>
        </label>

        <div class="actions">
          <span class="file-name">
            {{ files.length ? (multiple ? `已选择 ${files.length} 个文件` : files[0].name) : '未选择文件' }}
          </span>
          <div class="action-btns">
            <button v-if="files.length" class="btn ghost" :disabled="loading" @click="setFiles([])">
              清除
            </button>
            <button class="btn" :disabled="!files.length || loading" @click="submit">
              <AppIcon :name="loading ? 'clock' : 'bolt'" :size="15" />
              {{ loading ? '执行中…' : '开始执行' }}
            </button>
          </div>
        </div>

        <p v-if="error" class="alert error">{{ error }}</p>
      </article>

      <article class="card panel">
        <header class="card-head">
          <p class="card-title">执行结果</p>
          <div v-if="result" class="head-right">
            <p class="card-sub">
              {{ result.job_id }} ·
              <span :class="result.status === 'done' ? 'state ok' : 'state fail'">
                {{ result.status }}
              </span>
            </p>
            <button
              v-if="result.status === 'done' && result.final_report"
              class="btn ghost small"
              @click="download"
            >
              <AppIcon name="doc" :size="14" />
              下载
            </button>
          </div>
          <p v-else class="card-sub">尚未执行</p>
        </header>

        <div v-if="result" class="result">
          <div class="metrics">
            <span class="metric-pill">
              <AppIcon name="clock" :size="13" />
              {{ duration(result.duration_ms) }}
            </span>
            <span class="metric-pill">
              <AppIcon name="doc" :size="13" />
              {{ result.feature_id ?? feature.id }}
            </span>
            <span v-if="entries.length" class="metric-pill">
              <AppIcon name="layers" :size="13" />
              {{ thousand(entries.length) }} 条目
            </span>
            <span v-else-if="structuredRows.length" class="metric-pill">
              <AppIcon name="layers" :size="13" />
              {{ thousand(structuredRows.length) }} 字段
            </span>
            <span v-if="result.review_notes?.length" class="metric-pill warn">
              <AppIcon name="shield" :size="13" />
              {{ result.review_notes.length }} 个提示
            </span>
          </div>

          <p v-if="result.error" class="alert error">{{ result.error }}</p>

          <template v-if="result.status === 'done'">
            <nav class="segmented tabs">
              <button
                v-for="t in tabs"
                :key="t.key"
                :class="{ active: activeTab === t.key }"
                @click="activeTab = t.key"
              >
                {{ t.label }}
              </button>
            </nav>

            <div class="content-area">
              <div v-if="activeTab === 'report'" class="markdown">{{ result.final_report }}</div>

              <table v-if="activeTab === 'entries' && entries.length" class="table">
                <thead>
                  <tr>
                    <th class="w-level">层级</th>
                    <th class="w-num">编号</th>
                    <th>标题</th>
                    <th class="w-page">页码</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="(e, i) in entries" :key="i">
                    <td class="num">{{ e.level }}</td>
                    <td class="mono">{{ e.number }}</td>
                    <td>
                      <span :style="{ paddingLeft: `${(e.level - 1) * 14}px` }">{{ e.title }}</span>
                    </td>
                    <td class="num">{{ e.page || '' }}</td>
                  </tr>
                </tbody>
              </table>

              <table v-if="activeTab === 'structured' && structuredRows.length" class="table">
                <thead>
                  <tr>
                    <th>字段</th>
                    <th>值</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="row in structuredRows" :key="row.key">
                    <td class="key">{{ row.key }}</td>
                    <td>{{ row.value }}</td>
                  </tr>
                </tbody>
              </table>
              <p v-if="activeTab === 'structured' && !structuredRows.length" class="blank">
                未解析到结构化字段
              </p>

              <pre v-if="activeTab === 'ocr'" class="raw">{{ result.ocr_text }}</pre>
              <pre v-if="activeTab === 'translation'" class="raw">{{ result.translation }}</pre>
            </div>

            <div v-if="result.review_notes?.length" class="notes">
              <p class="notes-title">执行提示</p>
              <ul>
                <li v-for="(n, i) in result.review_notes" :key="i">{{ n }}</li>
              </ul>
            </div>
          </template>
        </div>

        <div v-else class="placeholder">
          <span class="place-icon"><AppIcon name="layers" :size="22" /></span>
          <p>执行后这里会展示生成结果、结构化条目与原始 OCR。</p>
        </div>
      </article>
    </div>
  </section>
</template>

<style scoped>
.page {
  display: flex;
  flex-direction: column;
  gap: 14px;
  max-width: 1240px;
}

.page-head {
  margin-bottom: 2px;
}

.page-title {
  font-size: 25px;
  letter-spacing: -0.03em;
}

.page-sub {
  margin-top: 3px;
  color: var(--text-2);
  font-size: 13px;
}

.pipeline {
  display: flex;
  align-items: center;
  gap: 9px;
  flex-wrap: wrap;
  padding: 13px 18px;
}

.pipeline-label {
  color: var(--text-3);
  font-size: 11.5px;
  font-weight: 700;
  letter-spacing: 0.05em;
  text-transform: uppercase;
  margin-right: 2px;
}

.pipeline-node {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 5px 11px;
  border-radius: 999px;
  background: var(--surface-2);
  color: var(--text);
  font-size: 12.5px;
  font-weight: 600;
}

.pipeline-arrow {
  color: var(--text-3);
  font-size: 12px;
}

.pipeline-tag {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  margin-left: auto;
  padding: 5px 11px;
  border-radius: 999px;
  background: var(--purple-soft);
  color: #8e3fbb;
  font-size: 12px;
  font-weight: 700;
}

.grid {
  display: grid;
  grid-template-columns: 380px minmax(0, 1fr);
  gap: 14px;
  align-items: start;
}

.panel {
  padding-bottom: 20px;
}

/* ---- 上传区 ---- */
.dropzone {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 7px;
  margin: 16px 20px;
  padding: 26px 18px;
  min-height: 208px;
  border: 1.5px dashed #d8d8e0;
  border-radius: var(--radius);
  background: var(--surface-2);
  cursor: pointer;
  text-align: center;
  transition: border-color 0.16s ease, background 0.16s ease;
}

.dropzone:hover,
.dropzone.dragging {
  border-color: var(--blue);
  background: var(--blue-soft);
}

.dropzone.filled {
  padding: 10px;
  min-height: 0;
}

.dropzone input {
  display: none;
}

.drop-icon {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 44px;
  height: 44px;
  border-radius: 14px;
  background: var(--surface);
  color: var(--blue);
  border: 1px solid var(--border);
}

.drop-title {
  font-size: 13.5px;
  font-weight: 650;
}

.drop-sub {
  color: var(--text-3);
  font-size: 12px;
}

/* ---- 多页缩略图 ---- */
.thumbs {
  display: flex;
  gap: 10px;
  width: 100%;
  overflow-x: auto;
  padding: 4px 2px;
}

.thumbs.multi {
  flex-wrap: wrap;
  max-height: 300px;
  overflow-y: auto;
  overflow-x: hidden;
}

.thumb {
  position: relative;
  flex: none;
  margin: 0;
  width: 104px;
  border-radius: 12px;
  overflow: hidden;
  background: var(--surface);
  border: 1px solid var(--border);
}

.thumb img {
  display: block;
  width: 100%;
  height: 120px;
  object-fit: cover;
}

.thumb figcaption {
  display: flex;
  align-items: center;
  gap: 5px;
  padding: 5px 7px;
  font-size: 11px;
  color: var(--text-2);
}

.thumb-index {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  min-width: 16px;
  height: 16px;
  padding: 0 4px;
  border-radius: 999px;
  background: #1d1d1f;
  color: #fff;
  font-size: 10px;
  font-weight: 700;
  flex: none;
}

.thumb-name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.thumb-remove {
  position: absolute;
  top: 5px;
  right: 5px;
  width: 20px;
  height: 20px;
  border-radius: 50%;
  background: rgba(0, 0, 0, 0.55);
  color: #fff;
  font-size: 14px;
  line-height: 1;
  display: inline-flex;
  align-items: center;
  justify-content: center;
}

.thumb-remove:hover {
  background: var(--red);
}

.actions {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 0 20px;
}

.file-name {
  color: var(--text-2);
  font-size: 12.5px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  min-width: 0;
}

.action-btns {
  display: flex;
  gap: 8px;
  flex: none;
}

.alert {
  margin: 14px 20px 0;
  padding: 11px 14px;
  border-radius: var(--radius-sm);
  font-size: 12.5px;
  font-weight: 600;
  line-height: 1.6;
}

.alert.error {
  background: var(--red-soft);
  color: #c2362c;
}

/* ---- 结果区 ---- */
.head-right {
  display: flex;
  align-items: center;
  gap: 10px;
}

.btn.small {
  padding: 6px 12px;
  font-size: 12.5px;
}

.result {
  padding: 16px 20px 0;
}

.metrics {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}

.metric-pill {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 5px 11px;
  border-radius: 999px;
  background: var(--surface-2);
  color: var(--text-2);
  font-size: 12px;
  font-weight: 600;
}

.metric-pill.warn {
  background: var(--orange-soft);
  color: #a86700;
}

.tabs {
  margin: 14px 0;
  flex-wrap: wrap;
}

.content-area {
  min-height: 220px;
}

.markdown {
  white-space: pre-wrap;
  line-height: 1.78;
  font-size: 13.5px;
  padding: 16px 18px;
  border-radius: var(--radius);
  background: var(--surface-2);
  max-height: 460px;
  overflow: auto;
}

.raw {
  white-space: pre-wrap;
  word-break: break-word;
  font-size: 12.5px;
  line-height: 1.7;
  padding: 16px 18px;
  border-radius: var(--radius);
  background: var(--surface-2);
  max-height: 460px;
  overflow: auto;
}

.table {
  width: 100%;
  border-collapse: collapse;
}

.table th {
  padding: 8px 10px;
  color: var(--text-3);
  font-size: 11.5px;
  font-weight: 700;
  text-align: left;
}

.table td {
  padding: 9px 10px;
  border-top: 1px solid var(--border);
  font-size: 13px;
  word-break: break-word;
}

.table .num,
.table .w-page {
  text-align: right;
  font-variant-numeric: tabular-nums;
}

.w-level {
  width: 58px;
}

.w-num {
  width: 96px;
}

.w-page {
  width: 72px;
}

.mono {
  font-variant-numeric: tabular-nums;
  font-size: 12.5px;
  color: var(--text-2);
}

.table .key {
  color: var(--text-2);
  font-weight: 600;
  white-space: nowrap;
}

.notes {
  margin-top: 14px;
  padding: 14px 16px;
  border-radius: var(--radius);
  background: var(--orange-soft);
}

.notes-title {
  font-size: 12.5px;
  font-weight: 700;
  color: #a86700;
  margin-bottom: 6px;
}

.notes ul {
  margin: 0;
  padding-left: 18px;
  color: #8a5600;
  font-size: 13px;
  line-height: 1.75;
}

.placeholder {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 10px;
  padding: 60px 20px;
  color: var(--text-3);
  font-size: 13px;
  text-align: center;
}

.place-icon {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 46px;
  height: 46px;
  border-radius: 15px;
  background: var(--surface-2);
  color: var(--text-3);
}

.state.ok {
  color: #1f9d4d;
  font-weight: 700;
}

.state.fail {
  color: #d13b30;
  font-weight: 700;
}

.blank {
  padding: 16px 0;
  color: var(--text-3);
  font-size: 13px;
}

@media (max-width: 1180px) {
  .grid {
    grid-template-columns: 1fr;
  }
}
</style>
