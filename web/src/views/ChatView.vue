<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import AppIcon from '../components/AppIcon.vue'
import { fetchModelCaps, fetchProviderModels, sendChat } from '../api'
import { duration } from '../chart'
import { renderMarkdown } from '../markdown'
import { pushLog } from '../logstore'
import type { FeatureInfo, ModelCaps, ModelInfo, ProviderInfo } from '../types'
import type { ChatImage, ChatMessage } from '../types'

const props = defineProps<{
  feature: FeatureInfo
  providers: ProviderInfo[]
}>()
const emit = defineEmits<{ (e: 'done'): void }>()

const STORAGE_KEY = 'go-ai-agent.chat.history.v1'
const MD_KEY = 'go-ai-agent.chat.markdown.v1'

// ---- provider / model 选择 ----
const providerName = ref(props.feature.chat?.default_provider || props.providers[0]?.name || '')
const models = ref<ModelInfo[]>([])
const model = ref('')
const modelsLoading = ref(false)
const modelsError = ref('')

// ---- 流式开关 ----
// 开关仅表示默认使用流式还是非流式；端点是否真的支持流式，
// 由后端在调用时检测，不支持时自动降级为一次性 JSON 并回传降级说明。
const wantStream = ref(props.feature.chat?.stream_default ?? true)
const fallbackNote = ref('')

// ---- 会话 ----
const messages = ref<ChatMessage[]>([])
const draft = ref('')
const sending = ref(false)
const showReasoning = ref(true)

/**
 * Markdown 渲染开关：默认开启。
 * 关闭后 assistant 气泡直接显示模型返回的原始文本（bubble 回到 pre-wrap），
 * 用于核对 Markdown 源码本身是否符合预期（表格、标题、缩进等）。
 * 偏好写入 localStorage，刷新后保持一致。
 */
function loadMdPref(): boolean {
  try {
    return localStorage.getItem(MD_KEY) !== 'off'
  } catch {
    return true
  }
}
const mdRender = ref(loadMdPref())
watch(mdRender, (on) => {
  try {
    localStorage.setItem(MD_KEY, on ? 'on' : 'off')
  } catch {
    /* 存储不可用时忽略 */
  }
})
const listEl = ref<HTMLElement | null>(null)
let abort: AbortController | null = null

// ---- 图片附件（仅视觉模型）----
const fileInput = ref<HTMLInputElement | null>(null)
const attachments = ref<ChatImage[]>([])
const dragging = ref(false)
const attachError = ref('')

const canSend = computed(
  () =>
    !!providerName.value &&
    !!model.value &&
    !sending.value &&
    // 只发图不问话也是合法的多模态输入
    (!!draft.value.trim() || attachments.value.length > 0),
)

async function loadModels() {
  if (!providerName.value) return
  modelsLoading.value = true
  modelsError.value = ''
  models.value = []
  try {
    const res = await fetchProviderModels(providerName.value)
    models.value = res.models
    if (!models.value.length) modelsError.value = '该端点未返回任何模型'
  } catch (e) {
    modelsError.value = e instanceof Error ? e.message : String(e)
  } finally {
    modelsLoading.value = false
  }
  // 当前选中模型不在新列表中时回退到列表首个
  if (!models.value.some((m) => m.id === model.value)) model.value = models.value[0]?.id ?? ''
}

/** 当前选中模型的能力信息（能力标签展示用；真实探测结果优先于列表启发式）。 */
const selectedModel = computed(() => {
  const base = models.value.find((m) => m.id === model.value) ?? null
  const caps = probedCaps.value
  if (base && caps && caps.model === model.value) {
    return { ...base, vision: caps.vision, reasoning: caps.reasoning }
  }
  return base
})

/** 下拉选项里的能力后缀（原生 select 无法渲染 chip，用文本补充）。 */
function optionSuffix(m: ModelInfo): string {
  const caps = [m.reasoning && '推理', m.vision && '视觉'].filter(Boolean)
  return caps.length ? `　[${caps.join('·')}]` : ''
}

/** 当前模型是否可接图像输入（后端按端点元数据 + 模型名判定，启发式）。 */
const visionReady = computed(() => !!selectedModel.value?.vision)
const maxImages = computed(() => props.feature.chat?.max_images ?? 4)
const maxImageMB = computed(() => props.feature.chat?.max_image_mb ?? 8)

function pickImages() {
  attachError.value = ''
  fileInput.value?.click()
}

function onFilePicked(e: Event) {
  const input = e.target as HTMLInputElement
  if (input.files?.length) void addImages(input.files)
  input.value = '' // 重置后同一文件可再次选择
}

function onDragOver(e: DragEvent) {
  if (!e.dataTransfer?.types?.includes('Files')) return
  // 始终接管文件拖放：非视觉模型也要在 drop 时给出提示，而不是让浏览器直接打开图片
  e.preventDefault()
  dragging.value = visionReady.value && !sending.value
}

function onDragLeave(e: DragEvent) {
  // 移到列表内子元素上也会触发 dragleave，只有真正离开区域才取消高亮
  const el = e.currentTarget as HTMLElement | null
  if (el && e.relatedTarget instanceof Node && el.contains(e.relatedTarget)) return
  dragging.value = false
}

function onDrop(e: DragEvent) {
  dragging.value = false
  if (!e.dataTransfer?.files?.length) return
  e.preventDefault() // 无论能否受理都不让浏览器直接打开图片
  if (!visionReady.value) {
    attachError.value = '当前模型未声明视觉能力，请先换用 VL / 多模态模型再上传图片'
    return
  }
  void addImages(e.dataTransfer.files)
}

/** 粘贴截图/图片同样转为附件（纯文本粘贴不受影响）。 */
function onPaste(e: ClipboardEvent) {
  const files = e.clipboardData?.files
  if (!files?.length) return
  if (!visionReady.value) {
    attachError.value = '当前模型未声明视觉能力，无法粘贴图片'
    return
  }
  e.preventDefault()
  void addImages(files)
}

/** 读入图片：过滤非图片、校验单张体积与单条数量上限（与后端 chat.max-* 一致）。 */
async function addImages(files: FileList | File[]) {
  attachError.value = ''
  const list = Array.from(files)
  const images = list.filter((f) => f.type.startsWith('image/'))
  const notes: string[] = []
  if (images.length !== list.length) notes.push('已忽略非图片文件')
  const maxBytes = maxImageMB.value * 1024 * 1024
  const next = [...attachments.value]
  for (const f of images) {
    if (next.length >= maxImages.value) {
      notes.push(`单条消息最多 ${maxImages.value} 张图片`)
      break
    }
    if (f.size > maxBytes) {
      notes.push(`${f.name} 超过 ${maxImageMB.value}MB，已跳过`)
      continue
    }
    try {
      next.push({ name: f.name || 'image', mime: f.type, data: await readAsBase64(f) })
    } catch {
      notes.push(`${f.name} 读取失败`)
    }
  }
  attachments.value = next
  attachError.value = notes.join('；')
}

/** FileReader → dataURL 的 base64 主体（前缀由服务端按 mime 拼装）。 */
function readAsBase64(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const fr = new FileReader()
    fr.onload = () => {
      const raw = String(fr.result ?? '')
      resolve(raw.includes(',') ? raw.slice(raw.indexOf(',') + 1) : raw)
    }
    fr.onerror = () => reject(fr.error ?? new Error('读取失败'))
    fr.readAsDataURL(file)
  })
}

function removeAttachment(i: number) {
  attachments.value = attachments.value.filter((_, k) => k !== i)
  attachError.value = ''
}

/** 缩略图 src；刷新恢复的附件 base64 已丢弃，返回空串（模板改渲染文件名标签）。 */
function imgSrc(img: ChatImage): string {
  return img.data ? `data:${img.mime};base64,${img.data}` : ''
}

/** 图片大图预览（点缩略图打开，点遮罩 / Esc 关闭）。 */
const previewSrc = ref('')

function onGlobalKeydown(e: KeyboardEvent) {
  if (e.key === 'Escape') previewSrc.value = ''
}

// ---- 模型能力探测（后端缓存：模型不变时直接返回缓存结果） ----
// 切换模型后异步探测真实能力（视觉/推理/流式），探测期间先用模型列表的启发式结论。
const probedCaps = ref<ModelCaps | null>(null)
const probingCaps = ref(false)

watch(
  [providerName, model],
  async ([p, m]) => {
    probedCaps.value = null
    if (!p || !m) return
    probingCaps.value = true
    try {
      const caps = await fetchModelCaps(p, m)
      probedCaps.value = caps
      // 同步刷新下拉列表里该模型的后缀，避免「顶部标签（真实探测）与下拉项
      // （拉取列表时的启发式）对同一模型给出矛盾结论」。
      const entry = models.value.find((x) => x.id === m)
      if (entry && caps.model === m) {
        entry.vision = caps.vision
        entry.reasoning = caps.reasoning
      }
    } catch {
      // 探测失败不阻塞对话：保持列表启发式结论
    } finally {
      probingCaps.value = false
    }
  },
  { immediate: true },
)

watch(model, () => {
  if (!visionReady.value && attachments.value.length) {
    attachments.value = []
    attachError.value = '已切换到非视觉模型，图片附件已清空'
  }
})

watch(providerName, () => {
  fallbackNote.value = ''
  void loadModels()
})

// ---- 会话持久化 ----
function persist() {
  try {
    // 图片的 base64 不写入 localStorage（单张就可达几 MB，配额极易溢出）：
    // 只保留文件名与类型，刷新后气泡内显示文件标签，该图不再参与后续轮次。
    const keep = messages.value
      .filter((m) => !m.error && !m.streaming)
      .slice(-100)
      .map((m) =>
        m.images?.length
          ? { ...m, images: m.images.map((img) => ({ name: img.name, mime: img.mime, data: '' })) }
          : m,
      )
    localStorage.setItem(STORAGE_KEY, JSON.stringify(keep))
  } catch {
    /* 存储不可用时忽略 */
  }
}

function restore() {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return
    const list = JSON.parse(raw) as ChatMessage[]
    messages.value = Array.isArray(list) ? list : []
  } catch {
    messages.value = []
  }
}

function clearConversation() {
  if (sending.value) stop()
  messages.value = []
  attachments.value = []
  attachError.value = ''
  previewSrc.value = ''
  persist()
}

/** 会话消息 → 请求片段（丢弃刷新后不再持有 base64 的图片）。 */
function toPart(m: ChatMessage): { role: string; content: string; images?: ChatImage[] } {
  const imgs = m.images?.filter((img) => !!img.data)
  return { role: m.role, content: m.content, images: imgs?.length ? imgs : undefined }
}

// ---- 发送 / 停止 ----
async function submit() {
  if (!canSend.value) return
  // 历史里的图片在本会话内存中仍持有 base64，追问可继续引用同一张图
  const history = messages.value.filter((m) => !m.error).map(toPart)
  const question = draft.value.trim()
  const images = attachments.value
  messages.value.push({
    role: 'user',
    content: question,
    images: images.length ? [...images] : undefined,
  })
  draft.value = ''
  attachments.value = []
  attachError.value = ''
  fallbackNote.value = ''
  const stream = wantStream.value
  // 捕获发送时使用的端点 / 模型快照：追加到回复气泡底部 meta，
  // 之后切换下拉选择不会影响已发送消息的展示。
  const usedSrc = `${providerName.value} / ${model.value}`
  messages.value.push({ role: 'assistant', content: '', streaming: stream })
  // 关键：必须取数组里的响应式代理元素继续修改；
  // 直接改 push 前的原始对象不会触发流式增量重渲染（只能等结束才整段显示）。
  const reply = messages.value[messages.value.length - 1]

  sending.value = true
  scrollToBottom()
  abort = new AbortController()
  pushLog('info', `[chat] 发送消息：${usedSrc}（${stream ? '流式' : '非流式'}）`)
  const payloadMessages = [
    ...history,
    { role: 'user' as const, content: question, images: images.length ? images : undefined },
  ]
  try {
    if (stream) {
      const res = await sendChat(
        {
          feature_id: props.feature.id,
          provider: providerName.value,
          model: model.value,
          messages: payloadMessages,
          stream: true,
        },
        {
          onDelta: (d) => {
            if (d.type === 'reasoning') reply.reasoning = (reply.reasoning ?? '') + d.text
            else reply.content += d.text
            scrollToBottom()
          },
          onDone: (done) => {
            reply.streaming = false
            reply.meta = `${duration(done.duration_ms)} · ↑${done.prompt_tokens} ↓${done.completion_tokens} tokens · ${usedSrc}`
            pushLog('info', `[chat] 生成完成：${duration(done.duration_ms)} · ${done.prompt_tokens + done.completion_tokens} tokens · ${usedSrc}`)
          },
        },
        abort.signal,
      )
      reply.streaming = false
      if (res) {
        // 后端检测到端点不支持流式，已自动降级为一次性 JSON：提示并关掉开关
        fallbackNote.value = res.note ?? '端点不支持流式，本次已自动改用非流式返回'
        wantStream.value = false
        reply.content = res.answer
        reply.reasoning = res.reasoning
        reply.meta = `${duration(res.duration_ms ?? 0)} · ↑${res.prompt_tokens} ↓${res.completion_tokens} tokens · ${usedSrc}`
      }
    } else {
      const res = await sendChat(
        {
          feature_id: props.feature.id,
          provider: providerName.value,
          model: model.value,
          messages: payloadMessages,
          stream: false,
        },
        {},
        abort.signal,
      )
      if (res) {
        reply.content = res.answer
        reply.reasoning = res.reasoning
        reply.meta = `↑${res.prompt_tokens} ↓${res.completion_tokens} tokens · ${usedSrc}`
      }
    }
    emit('done')
  } catch (e) {
    reply.streaming = false
    reply.error = e instanceof Error ? e.message : String(e)
    pushLog('error', `[chat] 生成失败：${reply.error}`)
  } finally {
    reply.meta = reply.meta ?? ''
    sending.value = false
    abort = null
    persist()
    scrollToBottom()
  }
}

function stop() {
  abort?.abort()
  abort = null
}

function scrollToBottom() {
  void nextTick(() => {
    if (listEl.value) listEl.value.scrollTop = listEl.value.scrollHeight
  })
}

function onKeydown(e: KeyboardEvent) {
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
    e.preventDefault()
    void submit()
  }
}

onMounted(async () => {
  restore()
  scrollToBottom()
  window.addEventListener('keydown', onGlobalKeydown)
  await loadModels()
})
onBeforeUnmount(() => {
  abort?.abort()
  window.removeEventListener('keydown', onGlobalKeydown)
})
</script>

<template>
  <section class="page">
    <header class="page-head">
      <div>
        <h1 class="page-title">{{ feature.name }}</h1>
        <p class="page-sub">{{ feature.description }}</p>
      </div>
      <button class="btn ghost small" :disabled="!messages.length" @click="clearConversation">
        <AppIcon name="refresh" :size="14" />
        新对话
      </button>
    </header>

    <div class="toolbar card">
      <label class="field">
        <span class="field-label">端点</span>
        <select v-model="providerName">
          <option v-for="p in providers" :key="p.name" :value="p.name">
            {{ p.name }}{{ p.default ? '（默认）' : '' }}
          </option>
        </select>
      </label>

      <label class="field grow">
        <span class="field-label">
          模型
          <button class="link-btn" :disabled="modelsLoading" @click.prevent="loadModels">
            <AppIcon name="refresh" :size="12" />
            {{ modelsLoading ? '加载中…' : '刷新列表' }}
          </button>
          <span v-if="selectedModel" class="caps">
            <span
              v-if="selectedModel.reasoning"
              class="cap-chip reasoning"
              title="模型声明支持推理/思考，思考过程将流式展示"
            >
              推理
            </span>
            <span
              v-if="selectedModel.vision"
              class="cap-chip vision"
              title="模型声明支持图像输入：可点「图片」选择、或拖拽 / 粘贴到对话区"
            >
              视觉
            </span>
            <span
              v-if="!selectedModel.reasoning && !selectedModel.vision"
              class="cap-chip plain"
              title="端点元数据与模型名均未声明特殊能力（启发式判定，仅供参考）"
            >
              纯文本
            </span>
            <span
              v-if="probingCaps"
              class="cap-chip probing"
              title="正在向端点真实探测视觉 / 推理 / 流式能力…"
            >
              探测中…
            </span>
          </span>
        </span>
        <select v-model="model" :disabled="modelsLoading || !models.length">
          <option v-if="!models.length" value="">
            {{ modelsError || (modelsLoading ? '加载中…' : '无可用模型') }}
          </option>
          <option v-for="m in models" :key="m.id" :value="m.id">{{ m.id }}{{ optionSuffix(m) }}</option>
        </select>
      </label>

      <div class="field stream-field">
        <span class="field-label">流式输出</span>
        <label class="switch" title="默认使用流式还是非流式返回；端点是否支持流式由调用时自动检测，不支持时自动降级">
          <input v-model="wantStream" type="checkbox" />
          <span class="track" />
        </label>
      </div>

      <p class="stream-state" :class="{ bad: !!fallbackNote }">
        <template v-if="fallbackNote">
          <AppIcon name="shield" :size="13" />
          {{ fallbackNote }}，流式开关已自动关闭
        </template>
        <template v-else-if="wantStream">
          <AppIcon name="bolt" :size="13" />
          默认流式返回 · 调用时自动检测，端点不支持时改用非流式
        </template>
        <template v-else> 默认非流式（一次性返回） </template>
      </p>
    </div>

    <p v-if="modelsError" class="alert error">{{ providerName }} 模型列表获取失败：{{ modelsError }}</p>

    <article
      class="card chat-card"
      :class="{ dropping: dragging }"
      @dragover="onDragOver"
      @dragleave="onDragLeave"
      @drop="onDrop"
    >
      <div ref="listEl" class="messages">
        <div v-if="!messages.length" class="placeholder">
          <span class="place-icon"><AppIcon name="chat" :size="22" /></span>
          <p>选择端点与模型，开始对话。{{ feature.input?.hint }}</p>
          <p v-if="visionReady" class="place-sub">该模型支持视觉，可将图片拖拽到此卡片上传</p>
        </div>

        <div v-for="(m, i) in messages" :key="i" class="msg" :class="m.role">
          <span class="avatar">
            <AppIcon :name="m.role === 'user' ? 'spark' : 'bolt'" :size="15" />
          </span>
          <div class="bubble-wrap">
            <details
              v-if="m.reasoning && showReasoning"
              class="reasoning"
              :open="!!m.streaming || undefined"
            >
              <summary>
                {{ m.streaming && !m.content ? '深度思考中' : '思考过程' }}
                {{ m.reasoning.length }} 字{{ m.streaming && !m.content ? '…' : '' }}
              </summary>
              <pre class="reasoning-text">{{ m.reasoning }}</pre>
            </details>
            <div
              class="bubble"
              :class="{
                typing: m.streaming && !m.content,
                md: mdRender && !!m.content && m.role === 'assistant',
              }"
            >
              <template v-if="m.content">
                <span
                  v-if="m.role === 'assistant' && mdRender"
                  v-html="renderMarkdown(m.content)"
                />
                <template v-else>{{ m.content }}</template>
              </template>
              <span v-else-if="m.streaming" class="dots">
                {{ m.reasoning ? '思考中，回答即将生成' : '正在生成' }}<span>…</span>
              </span>
              <span v-else-if="m.error" class="msg-error">{{ m.error }}</span>
              <span v-else-if="!m.images?.length" class="msg-empty">（空回复）</span>
              <!-- 用户消息附带的图片（放在 v-if 链之后，避免打断条件链） -->
              <div v-if="m.images?.length" class="bubble-imgs">
                <template v-for="(img, k) in m.images" :key="k">
                  <img
                    v-if="img.data"
                    :src="imgSrc(img)"
                    :alt="img.name"
                    :title="`${img.name}（点击查看大图）`"
                    @click="previewSrc = imgSrc(img)"
                  />
                  <span v-else class="img-chip" :title="`${img.name}：刷新后不再上传`">
                    <AppIcon name="image" :size="12" />
                    {{ img.name }}
                  </span>
                </template>
              </div>
            </div>
            <p v-if="m.meta" class="msg-meta">{{ m.meta }}</p>
          </div>
        </div>
      </div>

      <!-- 拖拽悬停提示（绝对定位，不占据消息流） -->
      <p v-if="dragging" class="drop-hint">
        <AppIcon name="image" :size="15" />
        松开以上传图片
      </p>

      <footer class="composer">
        <div v-if="attachments.length" class="attach-bar">
          <figure v-for="(a, i) in attachments" :key="i" class="attach">
            <img
              :src="imgSrc(a)"
              :alt="a.name"
              :title="`${a.name}（点击查看大图）`"
              @click="previewSrc = imgSrc(a)"
            />
            <button class="attach-x" type="button" title="移除这张图片" @click="removeAttachment(i)">
              <AppIcon name="x" :size="11" />
            </button>
            <figcaption :title="a.name">{{ a.name }}</figcaption>
          </figure>
          <p class="attach-count">{{ attachments.length }}/{{ maxImages }} 张 · 单张 ≤ {{ maxImageMB }}MB</p>
        </div>
        <p v-if="attachError" class="attach-error">{{ attachError }}</p>

        <div class="composer-row">
          <textarea
            v-model="draft"
            class="input"
            rows="2"
            placeholder="输入消息，Enter 发送，Shift+Enter 换行"
            :disabled="sending"
            @keydown="onKeydown"
            @paste="onPaste"
          />
          <div class="composer-actions">
            <label class="mini-check">
              <input v-model="showReasoning" type="checkbox" />
              显示思考
            </label>
            <label
              class="mini-switch"
              title="开启后按 Markdown 渲染回复；关闭则显示模型返回的原始文本"
            >
              <span>Markdown 渲染</span>
              <span class="switch">
                <input v-model="mdRender" type="checkbox" />
                <span class="track" />
              </span>
            </label>
            <button
              class="btn ghost small"
              :disabled="!visionReady || sending || attachments.length >= maxImages"
              :title="
                visionReady
                  ? `上传图片（最多 ${maxImages} 张，单张 ≤ ${maxImageMB}MB），也可直接拖拽或粘贴到对话区`
                  : '当前模型未声明视觉能力，换用 VL / 多模态模型后可上传图片'
              "
              @click="pickImages"
            >
              <AppIcon name="image" :size="14" />
              图片
            </button>
            <input
              ref="fileInput"
              class="file-hidden"
              type="file"
              accept="image/*"
              multiple
              @change="onFilePicked"
            />
            <button v-if="sending" class="btn ghost" @click="stop">停止</button>
            <button class="btn" :disabled="!canSend" @click="submit">
              <AppIcon :name="sending ? 'clock' : 'bolt'" :size="15" />
              {{ sending ? '生成中…' : '发送' }}
            </button>
          </div>
        </div>
      </footer>
    </article>

    <!-- 图片大图预览遮罩 -->
    <div
      v-if="previewSrc"
      class="lightbox"
      role="dialog"
      aria-modal="true"
      aria-label="图片预览"
      @click="previewSrc = ''"
    >
      <img :src="previewSrc" alt="图片预览" />
      <button class="lightbox-close" type="button" aria-label="关闭预览" @click.stop="previewSrc = ''">
        <AppIcon name="x" :size="14" />
      </button>
    </div>
  </section>
</template>

<style scoped>
.page {
  display: flex;
  flex-direction: column;
  gap: 14px;
  /* 宽度随窗口增长（超宽屏不至于挤成一条），高度铺满视口，
     使消息列表伸缩、输入区始终可见。 */
  width: 100%;
  max-width: 1180px;
  /* 100vh 减去 .content 上下内边距（26 + 64） */
  min-height: calc(100vh - 90px);
}

.page-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
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

.btn.small {
  padding: 7px 13px;
  font-size: 12.5px;
  flex: none;
}

/* ---- 工具栏 ---- */
.toolbar {
  display: flex;
  align-items: flex-end;
  gap: 14px;
  flex-wrap: wrap;
  padding: 14px 18px;
}

.field {
  display: flex;
  flex-direction: column;
  gap: 5px;
  min-width: 150px;
}

.field.grow {
  flex: 1;
  min-width: 220px;
}

.field-label {
  display: flex;
  align-items: center;
  gap: 8px;
  /* 统一标题行高度，保证各字段下拉框顶部对齐 */
  min-height: 24px;
  color: var(--text-3);
  font-size: 11.5px;
  font-weight: 700;
  letter-spacing: 0.04em;
}

.link-btn {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  color: var(--accent);
  font-size: 11px;
  font-weight: 600;
}

.link-btn:disabled {
  color: var(--text-3);
}

.field select {
  height: 34px;
  padding: 0 10px;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--surface);
  color: var(--text);
  font: inherit;
  font-size: 13px;
  max-width: 100%;
}

/* 选中模型的能力标签（标题行内） */
.caps {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  margin-left: 2px;
}

.cap-chip {
  padding: 0 8px;
  border-radius: 999px;
  border: 1px solid transparent;
  font-size: 10.5px;
  font-weight: 700;
  line-height: 1.6;
  letter-spacing: 0;
  white-space: nowrap;
}

.cap-chip.reasoning {
  color: var(--accent);
  background: var(--accent-bg);
  border-color: var(--accent);
}

.cap-chip.vision {
  color: var(--accent);
  background: var(--accent-bg);
  border-color: var(--accent);
}

.cap-chip.plain {
  color: var(--text-3);
  background: var(--surface-2);
  border-color: var(--border);
}

.cap-chip.probing {
  color: var(--text-2);
  background: var(--surface-2);
  border-color: var(--border);
  animation: cap-pulse 1.1s ease-in-out infinite;
}

@keyframes cap-pulse {
  0%,
  100% {
    opacity: 0.5;
  }
  50% {
    opacity: 1;
  }
}

.stream-field {
  flex-direction: row;
  align-items: center;
  gap: 9px;
  padding-bottom: 5px;
}

.switch {
  position: relative;
  display: inline-block;
  width: 38px;
  height: 22px;
  flex: none;
}

.switch input {
  opacity: 0;
  width: 0;
  height: 0;
}

.track {
  position: absolute;
  inset: 0;
  border-radius: 999px;
  background: var(--track);
  transition: background 0.16s ease;
}

.track::after {
  content: '';
  position: absolute;
  top: 2px;
  left: 2px;
  width: 18px;
  height: 18px;
  border-radius: 50%;
  background: #fff;
  box-shadow: 0 1px 3px rgba(0, 0, 0, 0.2);
  transition: transform 0.16s ease;
}

.switch input:checked + .track {
  background: var(--accent);
}

.switch input:checked + .track::after {
  transform: translateX(16px);
}

.switch.disabled {
  opacity: 0.5;
}

.stream-state {
  display: flex;
  align-items: center;
  gap: 5px;
  flex-basis: 100%;
  color: var(--text-3);
  font-size: 11.5px;
}

.stream-state.bad {
  color: var(--bad-strong);
}

.alert {
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

/* ---- 消息区 ---- */
.chat-card {
  display: flex;
  flex-direction: column;
  overflow: hidden;
  /* 整个卡片（消息区 + 输入区）都是图片拖放目标 */
  position: relative;
  /* 吃掉页面剩余高度，窗口拉大时列表跟着变高 */
  flex: 1;
  min-height: 0;
}

/* 拖拽图片悬停在卡片上时的高亮（outline 不占布局，不会引起滚动跳动） */
.chat-card.dropping {
  outline: 2px dashed var(--accent);
  outline-offset: -6px;
}

.drop-hint {
  position: absolute;
  top: 50%;
  left: 50%;
  transform: translate(-50%, -50%);
  z-index: 5;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  margin: 0;
  padding: 8px 16px;
  border-radius: 999px;
  background: var(--text);
  color: var(--bg);
  font-size: 12.5px;
  font-weight: 600;
  /* 提示条不能挡住 drop 目标 */
  pointer-events: none;
}

.place-sub {
  color: var(--text-3);
  font-size: 12px;
}

.messages {
  display: flex;
  flex-direction: column;
  gap: 16px;
  padding: 20px 20px 8px;
  /* 不再写死高度（min(52vh, 520px) 会把输入区顶到首屏之外），改为随卡片弹性伸缩 */
  flex: 1;
  min-height: 240px;
  overflow-y: auto;
}

.placeholder {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: 10px;
  flex: 1;
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

.msg {
  display: flex;
  gap: 10px;
  align-items: flex-start;
}

.msg.user {
  flex-direction: row-reverse;
}

.avatar {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  border-radius: 10px;
  flex: none;
  background: var(--surface-2);
  border: 1px solid var(--border);
  color: var(--text-2);
}

.msg.user .avatar {
  background: var(--text);
  border-color: var(--text);
  color: var(--bg);
}

.bubble-wrap {
  display: flex;
  flex-direction: column;
  gap: 5px;
  max-width: 78%;
  min-width: 0;
}

.msg.user .bubble-wrap {
  align-items: flex-end;
}

.bubble {
  padding: 10px 14px;
  border-radius: var(--r-sm);
  background: var(--surface-2);
  border: 1px solid var(--border);
  white-space: pre-wrap;
  word-break: break-word;
  font-size: 13.5px;
  line-height: 1.75;
}

/* AI 回复不用气泡：平铺排版，与正文一致 */
.msg.assistant .bubble {
  padding: 2px 0;
  background: transparent;
  border: none;
}

/* 气泡内的图片附件（缩略图点击无交互，仅展示） */
.bubble-imgs {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-top: 8px;
}

.bubble-imgs img {
  width: 132px;
  height: 132px;
  object-fit: cover;
  border-radius: var(--radius-sm);
  border: 1px solid var(--border);
  background: var(--surface);
  cursor: zoom-in;
}

/* 刷新后 base64 不再留存，只剩文件名标签 */
.img-chip {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 3px 9px;
  border: 1px dashed var(--border);
  border-radius: 999px;
  color: var(--text-3);
  font-size: 11.5px;
  max-width: 180px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* ---- Markdown 渲染（assistant 气泡，v-html 内容需用 :deep 命中）---- */
.bubble.md {
  white-space: normal;
}

.bubble.md :deep(:first-child) {
  margin-top: 0;
}

.bubble.md :deep(:last-child) {
  margin-bottom: 0;
}

.bubble.md :deep(p) {
  margin: 0 0 0.7em;
}

.bubble.md :deep(h1),
.bubble.md :deep(h2),
.bubble.md :deep(h3),
.bubble.md :deep(h4),
.bubble.md :deep(h5),
.bubble.md :deep(h6) {
  margin: 0.9em 0 0.5em;
  font-weight: 700;
  line-height: 1.4;
}

.bubble.md :deep(h1) {
  font-size: 1.4em;
}

.bubble.md :deep(h2) {
  font-size: 1.25em;
}

.bubble.md :deep(h3) {
  font-size: 1.12em;
}

.bubble.md :deep(ul),
.bubble.md :deep(ol) {
  margin: 0 0 0.7em;
  padding-left: 1.5em;
}

.bubble.md :deep(li) {
  margin: 0.2em 0;
}

.bubble.md :deep(li > ul),
.bubble.md :deep(li > ol) {
  margin: 0.2em 0;
}

.bubble.md :deep(a) {
  color: var(--accent);
  text-decoration: underline;
}

.bubble.md :deep(blockquote) {
  margin: 0 0 0.7em;
  padding: 0.2em 0.9em;
  border-left: 3px solid var(--border);
  color: var(--text-2);
}

.bubble.md :deep(code) {
  padding: 0.12em 0.4em;
  border-radius: 5px;
  background: rgba(127, 127, 127, 0.16);
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 0.88em;
}

.bubble.md :deep(pre) {
  margin: 0 0 0.7em;
  padding: 11px 13px;
  border-radius: var(--radius-sm);
  background: var(--text);
  color: #f5f5f7;
  overflow-x: auto;
}

.bubble.md :deep(pre code) {
  padding: 0;
  background: none;
  color: inherit;
  font-size: 0.85em;
  line-height: 1.6;
}

.bubble.md :deep(table) {
  width: 100%;
  margin: 0 0 0.7em;
  border-collapse: collapse;
  font-size: 0.94em;
}

.bubble.md :deep(th),
.bubble.md :deep(td) {
  padding: 5px 9px;
  border: 1px solid var(--border);
  text-align: left;
}

.bubble.md :deep(th) {
  background: var(--surface-2);
  font-weight: 700;
}

.bubble.md :deep(hr) {
  margin: 0.9em 0;
  border: none;
  border-top: 1px solid var(--border);
}

.bubble.md :deep(img) {
  max-width: 100%;
  border-radius: var(--radius-sm);
}

.bubble.typing {
  color: var(--text-3);
}

.dots span {
  animation: blink 1.1s infinite;
}

@keyframes blink {
  50% {
    opacity: 0.25;
  }
}

.msg-error {
  color: var(--bad-strong);
  font-weight: 600;
}

.msg-empty {
  color: var(--text-3);
}

.msg-meta {
  color: var(--text-3);
  font-family: var(--font-mono);
  font-size: 11px;
}

.reasoning {
  align-self: flex-start;
  max-width: 100%;
  padding-left: 10px;
  border-left: 2px solid var(--border);
  color: var(--text-2);
  font-size: 12px;
}

.reasoning summary {
  padding: 4px 0;
  cursor: pointer;
  user-select: none;
  font-weight: 600;
}

.reasoning-text {
  padding: 4px 0 6px;
  max-height: 180px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-word;
  color: var(--text-3);
  font-size: 11.5px;
  line-height: 1.7;
}

/* ---- 输入区 ---- */
.composer {
  display: flex;
  flex-direction: column;
  gap: 8px;
  flex: none;
  padding: 12px 20px 18px;
  border-top: 1px solid var(--border);
}

.composer-row {
  display: flex;
  align-items: flex-end;
  gap: 12px;
  /* 窄窗口下选项与按钮整行换行，避免开关被裁掉 */
  flex-wrap: wrap;
}

.composer-row .input {
  flex: 1 1 280px;
  min-width: 0;
  padding: 10px 13px;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  color: var(--text);
  font: inherit;
  font-size: 13.5px;
  line-height: 1.6;
  resize: vertical;
}

.composer-row .input:focus {
  outline: none;
  border-color: var(--accent);
  background: var(--surface);
}

/* 待发送图片缩略图行 */
.attach-bar {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  flex-wrap: wrap;
  padding: 8px 10px;
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--surface-2);
}

.attach {
  position: relative;
  display: flex;
  flex-direction: column;
  gap: 3px;
  width: 76px;
  margin: 0;
}

.attach img {
  width: 76px;
  height: 60px;
  object-fit: cover;
  border-radius: 8px;
  border: 1px solid var(--border);
  background: var(--surface);
  cursor: zoom-in;
}

.attach figcaption {
  color: var(--text-3);
  font-size: 10.5px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.attach-x {
  position: absolute;
  top: -6px;
  right: -4px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 18px;
  height: 18px;
  border-radius: 50%;
  background: var(--text);
  color: var(--bg);
  line-height: 1;
}

.attach-x:hover {
  background: var(--bad-strong);
}

.attach-count {
  align-self: center;
  margin: 0 0 0 auto;
  color: var(--text-3);
  font-size: 11.5px;
  white-space: nowrap;
}

.attach-error {
  margin: 0;
  color: var(--bad-strong);
  font-size: 12px;
  font-weight: 600;
}

/* 隐藏原生文件选择器，由「图片」按钮触发 */
.file-hidden {
  display: none;
}

/* ---- 图片大图预览 ---- */
.lightbox {
  position: fixed;
  inset: 0;
  z-index: 60;
  display: flex;
  align-items: center;
  justify-content: center;
  background: var(--scrim);
  cursor: zoom-out;
}

.lightbox img {
  max-width: 90vw;
  max-height: 88vh;
  border-radius: var(--radius);
  background: var(--surface);
  box-shadow: 0 18px 48px rgba(0, 0, 0, 0.35);
  cursor: default;
}

.lightbox-close {
  position: absolute;
  top: 22px;
  right: 26px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 32px;
  height: 32px;
  border-radius: 50%;
  background: rgba(255, 255, 255, 0.9);
  color: var(--text);
}

.composer-actions {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  flex: none;
}

.mini-check {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  color: var(--text-3);
  font-size: 12px;
  cursor: pointer;
}

/* 输入区的 Markdown 渲染开关（复用工具栏 .switch 样式，尺寸略缩） */
.mini-switch {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  color: var(--text-3);
  font-size: 12px;
  cursor: pointer;
}

.mini-switch .switch {
  width: 32px;
  height: 18px;
}

.mini-switch .track::after {
  width: 14px;
  height: 14px;
}

.mini-switch input:checked + .track::after {
  transform: translateX(14px);
}
</style>
