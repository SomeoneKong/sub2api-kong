<template>
  <BaseDialog :show="true" title="指纹测试" width="wide" @close="close">
    <div class="space-y-4">
      <div
        class="flex items-center gap-3 rounded-xl border border-gray-200 bg-gray-50 p-3 dark:border-dark-500 dark:bg-dark-700"
      >
        <div class="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-gradient-to-br from-primary-500 to-primary-600">
          <Icon name="beaker" size="md" class="text-white" :stroke-width="2" />
        </div>
        <div class="min-w-0">
          <div class="truncate font-semibold text-gray-900 dark:text-gray-100">{{ account.name }}</div>
          <span class="rounded bg-gray-200 px-1.5 py-0.5 text-[10px] font-medium uppercase text-gray-500 dark:bg-dark-500 dark:text-gray-400">
            {{ account.type }}
          </span>
        </div>
      </div>

      <div class="rounded-lg bg-amber-50 px-3 py-2 text-xs leading-relaxed text-amber-800 dark:bg-amber-500/10 dark:text-amber-300">
        <p>结果是这几次挑战观察到的相对信号，不是绝对档位判定：</p>
        <ul class="mt-1 list-disc space-y-0.5 pl-4">
          <li>闭集归因：指纹库以外的模型也会被归到最像的候选上；</li>
          <li>校准库取自不带 instructions 的环境，而测试请求带着 codex 的默认 instructions；</li>
          <li>各份挑战不带票，不保证落到同一个模型，各份指向不同时只报告观察到了差异；</li>
          <li>上游回报的模型名是上游的声明，与指纹是两类独立证据。</li>
        </ul>
      </div>

      <div class="space-y-1.5">
        <label class="text-sm font-medium text-gray-700 dark:text-gray-300">目标模型</label>
        <Select
          v-model="model"
          :options="targetOptions"
          :disabled="running || loadingTargets"
          :placeholder="loadingTargets ? '加载中…' : '选择目标模型'"
        />
        <p v-if="targetsError" class="text-xs text-red-600 dark:text-red-400">{{ targetsError }}</p>
        <p v-else class="text-xs text-gray-500 dark:text-gray-400">
          经账号自己的代理逐份发挑战，每份都是一次真实的上游请求；关闭弹窗即停止剩余挑战。
        </p>
      </div>

      <div v-if="testId || parts.length" class="space-y-2">
        <div class="flex items-baseline justify-between">
          <h4 class="text-sm font-medium text-gray-700 dark:text-gray-300">进度</h4>
          <span class="text-xs text-gray-500 dark:text-gray-400">{{ progressText }}</span>
        </div>
        <div
          v-for="p in parts"
          :key="p.index"
          class="rounded-lg border border-gray-200 p-3 text-xs dark:border-dark-600"
        >
          <div class="flex items-center justify-between">
            <span class="font-medium text-gray-800 dark:text-gray-200">第 {{ p.index }} 份</span>
            <span v-if="p.pending" class="flex items-center gap-1 text-gray-500 dark:text-gray-400">
              <Icon v-if="running" name="refresh" size="xs" class="animate-spin" :stroke-width="2" />
              {{ running ? '进行中…' : '未完成' }}
            </span>
            <span v-else :class="p.valid ? 'text-green-600 dark:text-green-400' : 'text-amber-600 dark:text-amber-400'">
              {{ partValidityText(p) }}
            </span>
          </div>
          <dl v-if="!p.pending" class="mt-2 grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-gray-600 dark:text-gray-400">
            <dt class="text-gray-500 dark:text-gray-500">状态码</dt>
            <dd class="font-mono">{{ p.status_code ?? '—' }}</dd>
            <dt class="text-gray-500 dark:text-gray-500">上游回报的模型</dt>
            <dd class="font-mono">{{ p.reported_model || '未回报' }}</dd>
            <dt class="text-gray-500 dark:text-gray-500">数字个数</dt>
            <dd class="font-mono">{{ p.digit_count ?? 0 }}</dd>
            <dt class="text-gray-500 dark:text-gray-500">本份归因</dt>
            <dd>{{ p.attribution ? nameOf(p.attribution, p.cumulative) : '—' }}</dd>
            <dt class="text-gray-500 dark:text-gray-500">累计分布</dt>
            <dd>
              <span v-if="!p.cumulative?.length">—</span>
              <span v-for="c in p.cumulative" :key="c.model" class="mr-3 inline-block whitespace-nowrap">
                {{ c.display_name || c.model }}
                <span class="font-mono">{{ formatProbability(c.probability) }}</span>
              </span>
            </dd>
            <template v-if="p.latency_ms">
              <dt class="text-gray-500 dark:text-gray-500">耗时</dt>
              <dd class="font-mono">
                {{ p.latency_ms }} ms<template v-if="p.output_tokens != null">，输出 {{ p.output_tokens }} token</template>
              </dd>
            </template>
            <template v-if="p.error">
              <dt class="text-gray-500 dark:text-gray-500">错误</dt>
              <dd class="break-all text-red-600 dark:text-red-400">{{ p.error }}</dd>
            </template>
          </dl>
        </div>
      </div>

      <div v-if="result" class="space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600">
        <div class="flex flex-wrap gap-x-6 gap-y-2 text-sm">
          <div class="flex items-center gap-2">
            <span class="text-gray-500 dark:text-gray-400">执行结果</span>
            <span v-if="execution" data-testid="fp-execution" :class="badgeClass(execution.tone)">{{ execution.text }}</span>
          </div>
          <div class="flex items-center gap-2">
            <span class="text-gray-500 dark:text-gray-400">模型结论</span>
            <span v-if="verdict" data-testid="fp-verdict" :class="badgeClass(verdict.tone)">{{ verdict.text }}</span>
            <span v-else data-testid="fp-verdict" class="text-xs text-gray-400 dark:text-gray-500">无（执行未完成）</span>
          </div>
        </div>
        <p class="text-xs text-gray-600 dark:text-gray-400">
          结束原因：{{ endReasonText(result.end_reason) }}<span v-if="result.detail" class="break-all">（{{ result.detail }}）</span>
        </p>
        <div v-if="result.candidates?.length" class="text-xs">
          <div class="mb-1 text-gray-500 dark:text-gray-400">最终分布（共用 {{ result.parts }} 份挑战）</div>
          <ul class="space-y-0.5 text-gray-700 dark:text-gray-300">
            <li v-for="c in result.candidates" :key="c.model" class="flex justify-between gap-4">
              <span class="truncate">{{ c.display_name || c.model }}</span>
              <span class="font-mono">{{ formatProbability(c.probability) }}</span>
            </li>
          </ul>
        </div>
        <p
          v-if="result.persist_error"
          class="rounded bg-amber-50 px-2 py-1 text-xs text-amber-800 dark:bg-amber-500/10 dark:text-amber-300"
        >
          有证据没写进库，事后读库还原不全：{{ result.persist_error }}
        </p>
        <p v-if="testId" class="text-[11px] text-gray-400 dark:text-gray-500">
          测试 ID：<span class="font-mono">{{ testId }}</span>
        </p>
      </div>

      <p v-if="errorText" class="text-sm text-red-600 dark:text-red-400">{{ errorText }}</p>
    </div>

    <template #footer>
      <div class="flex justify-end gap-3">
        <button
          class="rounded-lg bg-gray-100 px-4 py-2 text-sm font-medium text-gray-700 transition-colors hover:bg-gray-200 dark:bg-dark-600 dark:text-gray-300 dark:hover:bg-dark-500"
          @click="close"
        >
          关闭
        </button>
        <button
          :disabled="!canStart"
          :class="[
            'flex items-center gap-2 rounded-lg px-4 py-2 text-sm font-medium text-white transition-all',
            canStart ? 'bg-primary-500 hover:bg-primary-600' : 'cursor-not-allowed bg-primary-400'
          ]"
          @click="start"
        >
          <Icon :name="running ? 'refresh' : 'play'" size="sm" :class="{ 'animate-spin': running }" :stroke-width="2" />
          <span>{{ running ? '测试中…' : result || errorText ? '重新测试' : '开始' }}</span>
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Select from '@/components/common/Select.vue'
import { Icon } from '@/components/icons'
import { extractApiErrorMessage } from '@/utils/apiError'
import type { Account } from '@/types'
import { FingerprintTestHttpError, getFingerprintTargets, runFingerprintTest } from './api'
import {
  endReasonText,
  executionLabel,
  formatProbability,
  partValidityText,
  verdictLabel,
  type LabelTone
} from './labels'
import type {
  FingerprintCandidate,
  FingerprintPart,
  FingerprintResult,
  FingerprintTarget,
  FingerprintTestEvent
} from './types'

const props = defineProps<{ account: Account }>()
const emit = defineEmits<{ close: [] }>()

/** 进度里的一份；pending 表示已开始、还没收到它的结果。 */
interface PartView extends FingerprintPart {
  pending: boolean
}

const targets = ref<FingerprintTarget[]>([])
const loadingTargets = ref(true)
const targetsError = ref('')
const model = ref('')

const running = ref(false)
const testId = ref('')
const targetModel = ref('')
const maxParts = ref(0)
const parts = ref<PartView[]>([])
const result = ref<FingerprintResult | null>(null)
const errorText = ref('')
let controller: AbortController | null = null

const targetOptions = computed(() =>
  targets.value.map((t) => ({
    value: t.model,
    label: t.display_name && t.display_name !== t.model ? `${t.display_name}（${t.model}）` : t.model
  }))
)
const canStart = computed(() => !running.value && !loadingTargets.value && model.value !== '')
const execution = computed(() => (result.value ? executionLabel(result.value.execution) : null))
const verdict = computed(() => (result.value ? verdictLabel(result.value) : null))
const progressText = computed(() => {
  const finished = parts.value.filter((p) => !p.pending).length
  const target = targetModel.value ? `目标 ${targetModel.value}，` : ''
  return `${target}已完成 ${finished} 份${maxParts.value ? ` / 最多 ${maxParts.value} 份` : ''}`
})

const BADGE_TONE: Record<LabelTone, string> = {
  success: 'bg-green-100 text-green-700 dark:bg-green-500/20 dark:text-green-400',
  danger: 'bg-red-100 text-red-700 dark:bg-red-500/20 dark:text-red-400',
  warning: 'bg-amber-100 text-amber-700 dark:bg-amber-500/20 dark:text-amber-400',
  neutral: 'bg-gray-100 text-gray-600 dark:bg-gray-700 dark:text-gray-400'
}

function badgeClass(tone: LabelTone): string {
  return `rounded-full px-2.5 py-0.5 text-xs font-semibold ${BADGE_TONE[tone]}`
}

/** 模型的显示名：先找这一份的累计分布，再找目标列表，都没有就用模型 id。 */
function nameOf(modelId: string, candidates?: FingerprintCandidate[]): string {
  return (
    candidates?.find((c) => c.model === modelId)?.display_name ||
    targets.value.find((t) => t.model === modelId)?.display_name ||
    modelId
  )
}

function upsertPart(part: FingerprintPart, pending: boolean) {
  const view: PartView = { ...part, pending }
  const i = parts.value.findIndex((p) => p.index === part.index)
  if (i >= 0) parts.value[i] = view
  else parts.value.push(view)
}

function handleEvent(event: FingerprintTestEvent) {
  switch (event.type) {
    case 'started':
      testId.value = event.test_id
      targetModel.value = event.target_model
      maxParts.value = event.max_parts
      break
    case 'part_started':
      upsertPart(event.part, true)
      break
    case 'part':
      upsertPart(event.part, false)
      break
    case 'done':
      testId.value = event.test_id || testId.value
      result.value = event.result
      break
  }
}

function describeError(err: unknown): string {
  if (err instanceof FingerprintTestHttpError) return `发起失败（${err.status}）：${err.message}`
  if (err instanceof Error && err.message) return `测试中断：${err.message}`
  return '测试中断'
}

async function start() {
  if (!canStart.value) return
  abort()
  testId.value = ''
  targetModel.value = ''
  maxParts.value = 0
  parts.value = []
  result.value = null
  errorText.value = ''
  running.value = true

  const current = new AbortController()
  controller = current
  try {
    await runFingerprintTest(props.account.id, model.value, { signal: current.signal, onEvent: handleEvent })
    if (!result.value) errorText.value = '连接已结束，但没有收到结论（可能是网络中断）。'
  } catch (err) {
    if (current.signal.aborted) return
    errorText.value = describeError(err)
  } finally {
    if (controller === current) {
      controller = null
      running.value = false
    }
  }
}

function abort() {
  controller?.abort()
  controller = null
  running.value = false
}

function close() {
  abort()
  emit('close')
}

onMounted(async () => {
  try {
    targets.value = await getFingerprintTargets()
    model.value = targets.value[0]?.model ?? ''
    if (!targets.value.length) targetsError.value = '没有可选的目标模型。'
  } catch (err) {
    targetsError.value = `取目标模型失败：${extractApiErrorMessage(err, '未知错误')}`
  } finally {
    loadingTargets.value = false
  }
})

// 被卸载（离开账号页等）时同样中止：后端看到连接断开会停止剩余挑战。
onBeforeUnmount(abort)
</script>
