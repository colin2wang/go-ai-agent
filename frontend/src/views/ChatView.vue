<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import AppIcon from '../components/AppIcon.vue'
import { fetchProviderModels, sendChat } from '../api'
import { duration } from '../chart'
import type { FeatureInfo, ModelInfo, ProviderInfo } from '../types'
import type { ChatMessage } from '../types'

const props = defineProps<{
  feature: FeatureInfo
  providers: ProviderInfo[]
}>()
const emit = defineEmits<{ (e: 'done'): void }>()

const STORAGE_KEY = 'go-ai-agent.chat.history.v1'

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
const listEl = ref<HTMLElement | null>(null)
let abort: AbortController | null = null

const canSend = computed(
  () => !!providerName.value && !!model.value && !!draft.value.trim() && !sending.value,
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

/** 当前选中模型的能力信息（能力标签展示用）。 */
const selectedModel = computed(() => models.value.find((m) => m.id === model.value) ?? null)

/** 下拉选项里的能力后缀（原生 select 无法渲染 chip，用文本补充）。 */
function optionSuffix(m: ModelInfo): string {
  const caps = [m.reasoning && '推理', m.vision && '视觉'].filter(Boolean)
  return caps.length ? `　[${caps.join('·')}]` : ''
}

watch(providerName, () => {
  fallbackNote.value = ''
  void loadModels()
})

// ---- 会话持久化 ----
function persist() {
  try {
    const keep = messages.value.filter((m) => !m.error && !m.streaming)
    localStorage.setItem(STORAGE_KEY, JSON.stringify(keep.slice(-100)))
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
  persist()
}

// ---- 发送 / 停止 ----
async function submit() {
  if (!canSend.value) return
  const history: ChatMessage[] = messages.value
    .filter((m) => !m.error)
    .map((m) => ({ role: m.role, content: m.content }))
  const question = draft.value.trim()
  messages.value.push({ role: 'user', content: question })
  draft.value = ''
  fallbackNote.value = ''
  const stream = wantStream.value
  messages.value.push({ role: 'assistant', content: '', streaming: stream })
  // 关键：必须取数组里的响应式代理元素继续修改；
  // 直接改 push 前的原始对象不会触发流式增量重渲染（只能等结束才整段显示）。
  const reply = messages.value[messages.value.length - 1]

  sending.value = true
  scrollToBottom()
  abort = new AbortController()
  const payloadMessages = [...history, { role: 'user' as const, content: question }]
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
            reply.meta = `${duration(done.duration_ms)} · ↑${done.prompt_tokens} ↓${done.completion_tokens} tokens`
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
        reply.meta = `${duration(res.duration_ms ?? 0)} · ↑${res.prompt_tokens} ↓${res.completion_tokens} tokens`
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
        reply.meta = `↑${res.prompt_tokens} ↓${res.completion_tokens} tokens`
      }
    }
    emit('done')
  } catch (e) {
    reply.streaming = false
    reply.error = e instanceof Error ? e.message : String(e)
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
  await loadModels()
})
onBeforeUnmount(() => abort?.abort())
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
              title="模型声明支持图像输入（对话暂仅文本）"
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

    <article class="card chat-card">
      <div ref="listEl" class="messages">
        <div v-if="!messages.length" class="placeholder">
          <span class="place-icon"><AppIcon name="chat" :size="22" /></span>
          <p>选择端点与模型，开始对话。{{ feature.input?.hint }}</p>
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
            <div class="bubble" :class="{ typing: m.streaming && !m.content }">
              <template v-if="m.content">{{ m.content }}</template>
              <span v-else-if="m.streaming" class="dots">
                {{ m.reasoning ? '思考中，回答即将生成' : '正在生成' }}<span>…</span>
              </span>
              <span v-else-if="m.error" class="msg-error">{{ m.error }}</span>
              <span v-else class="msg-empty">（空回复）</span>
            </div>
            <p v-if="m.meta" class="msg-meta">{{ m.meta }}</p>
          </div>
        </div>
      </div>

      <footer class="composer">
        <textarea
          v-model="draft"
          class="input"
          rows="2"
          placeholder="输入消息，Enter 发送，Shift+Enter 换行"
          :disabled="sending"
          @keydown="onKeydown"
        />
        <div class="composer-actions">
          <label class="mini-check">
            <input v-model="showReasoning" type="checkbox" />
            显示思考
          </label>
          <button v-if="sending" class="btn ghost" @click="stop">停止</button>
          <button class="btn" :disabled="!canSend" @click="submit">
            <AppIcon :name="sending ? 'clock' : 'bolt'" :size="15" />
            {{ sending ? '生成中…' : '发送' }}
          </button>
        </div>
      </footer>
    </article>
  </section>
</template>

<style scoped>
.page {
  display: flex;
  flex-direction: column;
  gap: 14px;
  max-width: 980px;
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
  color: var(--blue);
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
  color: #6d28d9;
  background: #f3edfd;
  border-color: #e0d2f8;
}

.cap-chip.vision {
  color: #0369a1;
  background: #e7f4fc;
  border-color: #c9e6f7;
}

.cap-chip.plain {
  color: var(--text-3);
  background: var(--surface-2);
  border-color: var(--border);
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
  background: #d9d9e0;
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
  background: var(--green);
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
  color: #c2362c;
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
  color: #c2362c;
}

/* ---- 消息区 ---- */
.chat-card {
  display: flex;
  flex-direction: column;
  overflow: hidden;
}

.messages {
  display: flex;
  flex-direction: column;
  gap: 16px;
  padding: 20px 20px 8px;
  height: min(52vh, 520px);
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
  background: #1d1d1f;
  border-color: #1d1d1f;
  color: #fff;
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
  border-radius: 16px;
  background: var(--surface-2);
  border: 1px solid var(--border);
  white-space: pre-wrap;
  word-break: break-word;
  font-size: 13.5px;
  line-height: 1.75;
}

.msg.user .bubble {
  background: var(--blue-soft);
  border-color: transparent;
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
  color: #c2362c;
  font-weight: 600;
}

.msg-empty {
  color: var(--text-3);
}

.msg-meta {
  color: var(--text-3);
  font-size: 11px;
}

.reasoning {
  align-self: flex-start;
  max-width: 100%;
  border: 1px dashed var(--border);
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  color: var(--text-2);
  font-size: 12px;
}

.reasoning summary {
  padding: 6px 11px;
  cursor: pointer;
  user-select: none;
  font-weight: 600;
}

.reasoning-text {
  padding: 8px 12px 11px;
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
  align-items: flex-end;
  gap: 12px;
  padding: 12px 20px 18px;
  border-top: 1px solid var(--border);
}

.composer .input {
  flex: 1;
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

.composer .input:focus {
  outline: none;
  border-color: var(--blue);
  background: var(--surface);
}

.composer-actions {
  display: flex;
  align-items: center;
  gap: 10px;
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
</style>
