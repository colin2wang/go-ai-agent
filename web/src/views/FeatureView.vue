<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import AppIcon from '../components/AppIcon.vue'
import { fetchJob, fetchJobs, runFeatureAsync } from '../api'
import { pushLog } from '../logstore'
import { duration, thousand } from '../chart'
import type { FeatureInfo, JobResult, TOCEntry } from '../types'

const props = defineProps<{ feature: FeatureInfo }>()
const emit = defineEmits<{ (e: 'done'): void }>()

/** 节点名 → 展示标签与图标（未知节点兜底展示原名）。 */
const STEP_META: Record<string, { label: string; icon: string }> = {
  ocr: { label: 'OCR 提取', icon: 'scan' },
  merge: { label: '跨页合并', icon: 'layers' },
  parse: { label: '字幕解析', icon: 'doc' },
  render: { label: '结果输出', icon: 'doc' },
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

/** 后端下发的目标语言下拉框配置（未声明的子功能不显示）。 */
const langParam = computed(() => props.feature.input?.target_language ?? null)
const targetLang = ref(langParam.value?.default ?? '')

// ---- 批次大小（批量翻译类子功能）：单选预设 + 自定义输入（0-200），默认 50 ----
const BATCH_OPTIONS = ['20', '50', '100'] as const
const batchChoice = ref<'20' | '50' | '100' | 'custom'>('50')
const batchCustom = ref('')
const showBatch = computed(() => !!props.feature.pipeline?.includes('translate'))

/** 提交给后端的 batch_size：预设直接用；自定义取输入值（0-200 之外视为未提交）。 */
const batchSize = computed<number | null>(() => {
  if (!showBatch.value) return null
  if (batchChoice.value !== 'custom') return Number(batchChoice.value)
  const n = Math.floor(Number(batchCustom.value))
  if (!Number.isFinite(n) || n < 0 || n > 200) return null
  return n
})

/** 上传限制提示（取自后端下发的 input 上限，便于用户提前知情）。 */
const limitText = computed(() => {
  const inp = props.feature.input
  if (!inp?.max_files && !inp?.max_total_mb) return ''
  const parts: string[] = []
  if (inp.max_files) parts.push(`最多 ${inp.max_files} 个`)
  if (inp.max_file_mb) parts.push(`单个 ≤${inp.max_file_mb}MB`)
  if (inp.max_total_mb) parts.push(`总计 ≤${inp.max_total_mb}MB`)
  return parts.join(' · ')
})

interface Preview {
  name: string
  url: string
  /** 非图片文件（如 .srt）展示文件名卡片，避免 <img> 加载失败出现破图。 */
  isImage: boolean
}

const files = ref<File[]>([])
const previews = ref<Preview[]>([])
const dragging = ref(false)
const loading = ref(false)
const error = ref('')
const result = ref<JobResult | null>(null)
const activeTab = ref('report')

/** 异步执行中的进度快照（后端 jobs 表轮询回来）。 */
const progress = ref<JobResult | null>(null)
/** 本地计时（ms）：任务排队/执行中的已等待时长。 */
const elapsed = ref(0)
let timer = 0

/** 本子功能的历史任务（后端 jobs 表，按创建时间倒序）。 */
const jobs = ref<JobResult[]>([])

async function loadJobs() {
  try {
    jobs.value = await fetchJobs(props.feature.id, 20)
  } catch {
    /* 列表加载失败不阻塞页面 */
  }
}

/** 查看某条历史任务的完整结果。 */
async function viewJob(id: string) {
  error.value = ''
  try {
    result.value = await fetchJob(id)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

/**
 * 恢复未完成任务：切页回来（组件重新挂载）后，若该子功能仍有
 * pending/running 任务则继续轮询，直到终态后拉取完整结果。
 */
function resumeRunningJob() {
  if (loading.value) return
  const running = jobs.value.find((j) => j.status === 'pending' || j.status === 'running')
  if (!running) return
  pushLog('info', `发现未完成任务 ${running.job_id.slice(0, 8)}，继续跟踪进度`)
  loading.value = true
  progress.value = running
  elapsed.value = 0
  const startedAt = Date.now()
  timer = window.setInterval(() => (elapsed.value = Date.now() - startedAt), 500)
  void (async () => {
    try {
      for (;;) {
        const job = await fetchJob(running.job_id)
        progress.value = job
        if (job.status === 'done' || job.status === 'failed') {
          result.value = job
          if (job.status === 'done') {
            pushLog('info', `[${props.feature.id}] 恢复的任务 ${running.job_id.slice(0, 8)} 已完成`)
          } else {
            pushLog('error', `[${props.feature.id}] 任务 ${running.job_id.slice(0, 8)} 失败：${job.error || '未知原因'}`)
          }
          break
        }
        await new Promise((r) => setTimeout(r, 2000))
      }
      emit('done')
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
      progress.value = null
      window.clearInterval(timer)
      timer = 0
      void loadJobs()
    }
  })()
}

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
  previews.value = list.map((f) => ({
    name: f.name,
    url: URL.createObjectURL(f),
    isImage: f.type.startsWith('image/') || /\.(png|jpe?g|gif|webp|bmp)$/i.test(f.name),
  }))
  reset()
}

/**
 * 追加语义：多选模式下点击选择、拖拽、多次 Ctrl+V 均累加图片（不覆盖已选的）；
 * 单文件模式仍只保留本次的第一张。
 */
function addFiles(list: File[]) {
  if (!list.length) return
  setFiles(multiple.value ? [...files.value, ...list] : list.slice(0, 1))
}

/** 按 accept 白名单判断文件是否受理（扩展名 / image/* 前缀 / 具体 MIME）。 */
function acceptMatches(file: File): boolean {
  const types = accept.value
    .split(',')
    .map((t) => t.trim())
    .filter(Boolean)
  if (!types.length) return file.type.startsWith('image/')
  return types.some((t) => {
    if (t.startsWith('.')) return file.name.toLowerCase().endsWith(t.toLowerCase())
    if (t.endsWith('/*')) return file.type.startsWith(t.slice(0, -1))
    return file.type === t
  })
}

/**
 * 提交前的本地预校验：按后端下发的 input 上限提前拦截，
 * 避免大体积上传走完网络才被服务端拒绝（服务端仍会重复强校验）。
 */
function validateFiles(): string {
  const inp = props.feature.input
  const maxFiles = inp?.max_files ?? 0
  const maxFileMB = inp?.max_file_mb ?? 0
  const maxTotalMB = inp?.max_total_mb ?? 0
  if (maxFiles && files.value.length > maxFiles) {
    return `最多上传 ${maxFiles} 个文件（已选 ${files.value.length} 个）`
  }
  let total = 0
  for (let i = 0; i < files.value.length; i++) {
    const f = files.value[i]
    const mb = f.size / 1048576
    total += mb
    if (maxFileMB && mb > maxFileMB) {
      return `第 ${i + 1} 个文件 ${mb.toFixed(1)}MB 超过单文件上限 ${maxFileMB}MB`
    }
  }
  if (maxTotalMB && total > maxTotalMB) {
    return `上传总体积 ${total.toFixed(1)}MB 超过上限 ${maxTotalMB}MB`
  }
  return ''
}

/**
 * 全局粘贴：组件挂载期间监听 window 的 paste 事件，从剪贴板提取图片
 * （按 accept 白名单过滤 MIME），自动重命名为「粘贴图片-N.png」后追加。
 */
function onPaste(e: ClipboardEvent) {
  const imgs = Array.from(e.clipboardData?.files ?? []).filter(acceptMatches)
  if (!imgs.length) return
  e.preventDefault()
  let n = files.value.length
  const renamed = imgs.map((f) => {
    n += 1
    const ext = (f.type.split('/')[1] || 'png').replace('jpeg', 'jpg')
    // 剪贴板图片通常无有效文件名；已有扩展名的文件（如 .srt）保留原名
    const name = /\.[a-z0-9]+$/i.test(f.name) ? f.name : `粘贴图片-${n}.${ext}`
    return new File([f], name, { type: f.type })
  })
  addFiles(renamed)
}

function onFileChange(e: Event) {
  const input = e.target as HTMLInputElement
  addFiles(Array.from(input.files ?? []))
  input.value = ''
}

function onDrop(e: DragEvent) {
  dragging.value = false
  addFiles(Array.from(e.dataTransfer?.files ?? []))
}

function removeAt(index: number) {
  const next = files.value.filter((_, i) => i !== index)
  setFiles(next)
}

async function submit() {
  if (!files.value.length || loading.value) return
  const invalid = validateFiles()
  if (invalid) {
    error.value = invalid
    return
  }
  loading.value = true
  reset()
  elapsed.value = 0
  const startedAt = Date.now()
  timer = window.setInterval(() => (elapsed.value = Date.now() - startedAt), 500)
  pushLog('info', `[${props.feature.id}] 开始执行：${files.value.length} 个文件`)
  try {
    // 后端启用 persist-jobs 时为异步：下方轮询回调逐次刷新 progress；
    // 未启用时首次调用即返回终态，行为与旧版一致。
    // 声明了目标语言下拉框的子功能把选中值作为 target_lang 表单字段提交。
    const extra: Record<string, string> = {}
    if (langParam.value && targetLang.value) extra.target_lang = targetLang.value
    if (batchSize.value !== null) extra.batch_size = String(batchSize.value)
    result.value = await runFeatureAsync(
      props.feature.id,
      files.value,
      (job) => {
        progress.value = job
      },
      undefined,
      extra,
    )
    if (result.value.status === 'done') {
      pushLog('info', `[${props.feature.id}] 执行完成（${duration(result.value.duration_ms)}）`)
    } else if (result.value.status === 'failed') {
      pushLog('error', `[${props.feature.id}] 执行失败：${result.value.error || '未知原因'}`)
    }
    emit('done')
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
    pushLog('error', `[${props.feature.id}] 提交失败：${error.value}`)
  } finally {
    loading.value = false
    progress.value = null
    window.clearInterval(timer)
    timer = 0
    void loadJobs()
  }
}

/** 进度百分比（无 total 时返回 null，模板降级为不确定态动画）。 */
const progressPct = computed<number | null>(() => {
  const p = progress.value
  if (!p || !p.total || p.total <= 0) return null
  return Math.min(100, Math.round(((p.progress ?? 0) / p.total) * 100))
})

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

onMounted(() => {
  window.addEventListener('paste', onPaste)
  // 先拉历史任务，再从中恢复可能仍在执行的任务（切页回来不丢进度）
  void loadJobs().then(resumeRunningJob)
})
onBeforeUnmount(() => {
  window.removeEventListener('paste', onPaste)
  clearPreviews()
  if (timer) window.clearInterval(timer)
})
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
        <span class="pipeline-node">{{ s.name }}</span>
        <span v-if="i < pipeline.length - 1" class="pipeline-arrow">─▶</span>
      </template>
      <span v-if="feature.output?.template" class="pipeline-tag">
        <AppIcon name="doc" :size="13" />
        模板 {{ feature.output.template }}
      </span>
    </div>

    <div class="grid">
      <article class="card panel">
        <header class="card-head">
          <p class="card-title">{{ multiple ? '上传图片' : '上传文件' }}</p>
          <p class="card-sub">
            {{ multiple ? `已选 ${files.length} 张` : '单文件' }}
            <span v-if="limitText"> · {{ limitText }}</span>
          </p>
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
              {{ multiple ? '拖拽多张图片到此处，点击选择，或 Ctrl+V 粘贴' : '拖拽文件到此处，点击选择，或 Ctrl+V 粘贴' }}
            </span>
            <span class="drop-sub">{{ hint }}</span>
          </template>
          <div v-else class="thumbs" :class="{ multi: multiple }">
            <figure
              v-for="(p, i) in previews"
              :key="p.url"
              class="thumb"
              :class="{ 'file-fig': !p.isImage }"
            >
              <img v-if="p.isImage" :src="p.url" :alt="p.name" />
              <div v-else class="thumb-file">
                <AppIcon name="doc" :size="18" />
                <span class="thumb-file-name" :title="p.name">{{ p.name }}</span>
              </div>
              <figcaption v-if="p.isImage">
                <span v-if="multiple" class="thumb-index">{{ i + 1 }}</span>
                <span class="thumb-name">{{ p.name }}</span>
              </figcaption>
              <button type="button" class="thumb-remove" title="移除" @click.prevent.stop="removeAt(i)">
                ×
              </button>
            </figure>
          </div>
        </label>

        <div v-if="langParam" class="field-row">
          <label class="field-label" for="target-lang">{{ langParam.label || '目标语言' }}</label>
          <select id="target-lang" v-model="targetLang" class="field-select" :disabled="loading">
            <option v-for="opt in langParam.options" :key="opt" :value="opt">{{ opt }}</option>
          </select>
        </div>        <div v-if="showBatch" class="field-row">
          <span class="field-label">批次大小（每批送模型翻译的字幕条数）</span>
          <div class="batch-group" role="radiogroup" aria-label="批次大小">
            <label v-for="opt in BATCH_OPTIONS" :key="opt" class="batch-opt">
              <input
                v-model="batchChoice"
                type="radio"
                name="batch-size"
                :value="String(opt)"
                :disabled="loading"
              />
              <span class="num">{{ opt }}</span>
            </label>
            <label class="batch-opt custom">
              <input
                v-model="batchChoice"
                type="radio"
                name="batch-size"
                value="custom"
                :disabled="loading"
              />
              <span>自定义</span>
              <input
                v-model="batchCustom"
                class="batch-input num"
                type="number"
                min="0"
                max="200"
                step="1"
                placeholder="0-200"
                :disabled="loading || batchChoice !== 'custom'"
                @focus="batchChoice = 'custom'"
              />
            </label>
          </div>
          <span class="field-hint">批次越大总耗时越短，但单次译文更易被截断后触发逐条重译</span>
        </div>

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
          <p v-else class="card-sub">{{ loading ? '执行中' : '尚未执行' }}</p>
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

        <div v-else-if="loading" class="running">
          <header class="running-head">
            <p class="running-title">{{ progress?.detail || '任务执行中…' }}</p>
            <p class="running-elapsed">已等待 {{ duration(elapsed) }}</p>
          </header>
          <div class="bar">
            <div
              class="bar-fill"
              :class="{ indeterminate: progressPct === null }"
              :style="progressPct !== null ? { width: `${progressPct}%` } : {}"
            />
          </div>
          <p class="running-sub">
            <template v-if="progressPct !== null">
              进度 {{ progress?.progress ?? 0 }} / {{ progress?.total }}（{{ progressPct }}%）
            </template>
            <template v-else>高分辨率扫描件逐页识别较慢，可安心等待</template>
          </p>
          <p class="running-hint">
            {{ feature.async ? '任务已落库，刷新或切换到其他页面不会中断执行。' : '本页同步等待结果返回。' }}
          </p>
        </div>

        <div v-else-if="jobs.length" class="history">
          <p class="history-title">历史任务</p>
          <button
            v-for="j in jobs"
            :key="j.job_id"
            type="button"
            class="history-row"
            :disabled="j.status === 'pending' || j.status === 'running'"
            @click="viewJob(j.job_id)"
          >
            <span class="history-id num">{{ j.job_id.slice(0, 8) }}</span>
            <span class="state num" :class="j.status === 'done' ? 'ok' : j.status === 'failed' ? 'fail' : 'wait'">
              {{ j.status }}
            </span>
            <span class="history-detail" :title="j.detail">{{ j.detail || '—' }}</span>
            <span class="history-time num">{{ (j.updated_at || j.created_at || '').slice(5, 16) }}</span>
          </button>
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
  color: var(--text-2);
  font-family: var(--font-mono);
  font-size: 12px;
}

.pipeline-arrow {
  color: var(--text-3);
  font-size: 11px;
}

.pipeline-tag {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  margin-left: auto;
  color: var(--text-3);
  font-family: var(--font-mono);
  font-size: 11.5px;
}

.grid {
  display: grid;
  grid-template-columns: 1fr;
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
  border: 1px dashed var(--border);
  border-radius: var(--r-sm);
  background: transparent;
  cursor: pointer;
  text-align: center;
  transition: border-color 0.12s ease;
}

.dropzone:hover,
.dropzone.dragging {
  border-color: var(--accent);
}

.dropzone.filled {
  padding: 10px;
  min-height: 0;
}

.dropzone input {
  display: none;
}

/* ---- 目标语言选择：标签在上、下拉通栏 ---- */
.field-row {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 0 20px;
  margin-bottom: 14px;
}

.field-label {
  color: var(--text-2);
  font-size: 12px;
}

.field-select {
  height: 34px;
  width: 100%;
  padding: 0 10px;
  border: 1px solid var(--border);
  border-radius: var(--r-sm);
  background: var(--surface);
  color: var(--text);
  font: inherit;
  font-size: 13px;
}

.field-select:focus {
  outline: none;
  border-color: var(--accent);
}

.field-hint {
  color: var(--text-3);
  font-size: 11.5px;
}

/* ---- 批次大小单选组 ---- */
.batch-group {
  display: flex;
  align-items: center;
  gap: 14px;
  flex-wrap: wrap;
}

.batch-opt {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  color: var(--text-2);
  font-size: 12.5px;
  cursor: pointer;
}

.batch-opt input[type='radio'] {
  accent-color: var(--accent);
  margin: 0;
}

.batch-opt.custom .batch-input {
  width: 76px;
  height: 26px;
  padding: 0 8px;
  border: 1px solid var(--border);
  border-radius: var(--r-sm);
  background: var(--surface);
  color: var(--text);
  font-size: 12.5px;
}

.batch-opt.custom .batch-input:disabled {
  background: var(--surface-2);
  color: var(--text-3);
}

.batch-opt.custom .batch-input:focus {
  outline: none;
  border-color: var(--accent);
}

.drop-icon {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 44px;
  height: 44px;
  border-radius: var(--r-sm);
  background: var(--surface);
  color: var(--accent);
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

/* 非图片文件（如 .srt）：整行横排文件卡，文件名单行省略 */
.file-fig {
  width: 100%;
}

.thumb-file {
  display: flex;
  align-items: center;
  gap: 9px;
  padding: 10px 34px 10px 12px;
  color: var(--text-2);
}

.thumb-file-name {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 12.5px;
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
  background: var(--text);
  color: var(--bg);
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
  color: var(--bad-strong);
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
  color: var(--warn-strong);
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
  color: var(--warn-strong);
  margin-bottom: 6px;
}

.notes ul {
  margin: 0;
  padding-left: 18px;
  color: var(--warn-deep);
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

/* ---- 历史任务列表 ---- */
.history {
  padding: 8px 20px 16px;
  display: flex;
  flex-direction: column;
}

.history-title {
  color: var(--text-3);
  font-size: 11.5px;
  font-weight: 600;
  letter-spacing: 0.04em;
  padding: 8px 0 4px;
}

.history-row {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  padding: 8px 0;
  border-top: 1px solid var(--border);
  text-align: left;
  font-size: 12.5px;
  color: var(--text-2);
}

.history-row:hover:not(:disabled) {
  color: var(--text);
}

.history-row:disabled {
  cursor: default;
}

.history-id {
  flex: none;
}

.state {
  flex: none;
  font-size: 11.5px;
}

.state.ok {
  color: var(--ok);
}

.state.fail {
  color: var(--err);
}

.state.wait {
  color: var(--text-3);
}

.history-detail {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.history-time {
  flex: none;
  color: var(--text-3);
  font-size: 11.5px;
}

/* ---- 执行中进度 ---- */
.running {
  margin: 16px 20px 0;
  padding: 16px 18px;
  border-radius: var(--radius);
  background: var(--surface-2);
}

.running-head {
  display: flex;
  align-items: center;
  gap: 9px;
}

.running-title {
  font-size: 13px;
  font-weight: 650;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.running-elapsed {
  margin-left: auto;
  flex: none;
  color: var(--text-3);
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}

.bar {
  margin-top: 12px;
  height: 3px;
  border-radius: 2px;
  background: var(--border);
  overflow: hidden;
}

.bar-fill {
  height: 100%;
  background: var(--accent);
  transition: width 0.4s ease;
}

/* 总步数未知时的滑动动画 */
.bar-fill.indeterminate {
  width: 34%;
  animation: slide 1.3s ease-in-out infinite;
}

@keyframes slide {
  0% {
    transform: translateX(-100%);
  }
  100% {
    transform: translateX(300%);
  }
}

.running-sub {
  margin-top: 9px;
  color: var(--text-2);
  font-size: 12.5px;
  font-variant-numeric: tabular-nums;
}

.running-hint {
  margin-top: 3px;
  color: var(--text-3);
  font-size: 12px;
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
  color: var(--ok-strong);
  font-weight: 700;
}

.state.fail {
  color: var(--bad-strong);
  font-weight: 700;
}

.blank {
  padding: 16px 0;
  color: var(--text-3);
  font-size: 13px;
}

@media (max-width: 1180px) {
  .grid-metrics {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
  .grid-two {
    grid-template-columns: 1fr;
  }
}
</style>
