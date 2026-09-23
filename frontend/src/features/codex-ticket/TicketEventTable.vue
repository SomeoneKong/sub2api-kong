<!--
  事件表。总览页与单账号明细页共用——两处看的是同一份事件流，各写一份的话每次改动（比如给取票
  事件加耗时）都得改两遍，漏一边就变成两种口径。

  固定了 accountId 时它是「这个账号的事件」：账号筛选器与账号列一起收起来，那一列全是同一个值。
-->
<template>
  <section class="card px-4 py-4 sm:px-6">
    <div class="mb-3 flex flex-wrap items-center gap-2">
      <h2 class="mr-auto text-sm font-semibold text-gray-900 dark:text-white">{{ title }}</h2>
      <!-- 选项为空但已经选了值时仍要显示：`v-if` 只看选项数会把一个仍在生效的筛选条件连同它的
           清除入口一起藏掉——票被过期清理后 modelOptions 变空，而 modelFilter 里那个模型还在，
           "查询"仍带着它，页面上却再也没有地方能取消。 -->
      <EventFilterSelect
        v-if="!accountId && (accountOptions.length || accountFilter.length)"
        v-model="accountFilter"
        class="w-48"
        all-label="全部账号"
        :options="accountOptions"
        @change="load(0)"
      />
      <EventFilterSelect
        v-if="modelOptions.length || modelFilter.length"
        v-model="modelFilter"
        class="w-44"
        all-label="全部模型"
        :options="modelOptions"
        @change="load(0)"
      />
      <EventFilterSelect
        v-model="typeFilter"
        class="w-48"
        all-label="全部事件类型"
        :options="EVENT_TYPE_OPTIONS"
        @change="load(0)"
      />
      <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="load(0)">查询</button>
    </div>

    <p v-if="loadError" role="alert" class="py-4 text-sm text-red-600 dark:text-red-400">{{ loadError }}</p>
    <p v-else-if="events.length === 0" class="py-6 text-center text-sm text-gray-500 dark:text-dark-400">没有事件。</p>
    <div v-else class="overflow-x-auto">
      <table class="min-w-full text-sm">
        <thead class="text-left text-xs uppercase tracking-wider text-gray-500 dark:text-dark-400">
          <tr>
            <th class="px-3 py-2">时间</th>
            <th v-if="!accountId" class="px-3 py-2">账号</th>
            <th class="px-3 py-2">模型</th>
            <th class="px-3 py-2">类型</th>
            <th class="px-3 py-2">结果</th>
            <th class="px-3 py-2">出口</th>
            <th class="px-3 py-2">明细</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
          <template v-for="ev in events" :key="ev.id">
            <tr class="align-top">
              <td class="whitespace-nowrap px-3 py-2 text-xs text-gray-600 dark:text-dark-300">{{ formatTime(ev.created_at) }}</td>
              <td v-if="!accountId" class="px-3 py-2 text-xs text-gray-600 dark:text-dark-300">{{ accountName(ev.account_id) }}</td>
              <td class="px-3 py-2 text-xs text-gray-600 dark:text-dark-300">{{ ev.model || '—' }}</td>
              <td class="px-3 py-2 text-xs font-mono text-gray-700 dark:text-dark-200">{{ ev.event_type }}</td>
              <td class="px-3 py-2">
                <span class="badge" :class="outcomeClass(ev.outcome)">{{ ev.outcome }}</span>
                <span v-if="ev.status_code" class="ml-1 text-xs text-gray-500 dark:text-dark-400">HTTP {{ ev.status_code }}</span>
              </td>
              <td class="px-3 py-2 text-xs text-gray-500 dark:text-dark-400">
                票 <span :title="egressTitle(ev.ticket_egress)">{{ egressLabel(ev.ticket_egress) }}</span>
                / 流量 <span :title="egressTitle(ev.traffic_egress)">{{ egressLabel(ev.traffic_egress) }}</span>
                <span v-if="ev.idle_seconds !== null"> · 本系统记录空闲 {{ ev.idle_seconds }}s</span>
              </td>
              <td class="px-3 py-2 text-xs text-gray-500 dark:text-dark-400">
                <span v-if="ev.state_len !== null">len={{ ev.state_len }} </span>
                <!-- stg0 事件里的 model 是上游自己回报的，不是指纹归因的产物：标签要区分，
                     否则两种证据强度完全不同的结论在事件流里长得一样。 -->
                <span v-if="stgOf(ev) === 0 && ev.fingerprint_model" class="whitespace-nowrap">
                  上游回报 {{ ev.fingerprint_model }}
                </span>
                <span v-else-if="topModelsOf(ev).length" class="whitespace-nowrap">
                  归因
                  <span
                    v-for="(tm, i) in topModelsOf(ev)"
                    :key="tm.model"
                    :class="i === 0 ? 'font-medium text-gray-700 dark:text-dark-200' : ''"
                  >{{ i > 0 ? ' · ' : ' ' }}{{ tm.model }} {{ tm.p.toFixed(3) }}</span>
                </span>
                <span v-else-if="ev.fingerprint_model">归因 {{ ev.fingerprint_model }} </span>
                <!-- 耗时单独提出来：事件的时间列是请求**完成**时刻，同一批的两条 fetch 因此
                     按各自耗时先后排开，看起来像串行。把耗时摆在旁边，减回发起时刻就知道
                     它们是同时发出的。 -->
                <span v-if="durationOf(ev) !== null" class="whitespace-nowrap">
                  耗时 {{ durationText(durationOf(ev)!) }}
                </span>
                <span v-if="ev.detail && compactDetail(ev.detail)" class="font-mono">{{ compactDetail(ev.detail) }}</span>
                <button
                  v-if="verificationIDOf(ev)"
                  type="button"
                  class="btn btn-secondary btn-sm ml-2"
                  @click="toggleProbes(ev)"
                >
                  {{ expandedEventID === ev.id ? '收起明细' : '探测明细' }}
                </button>
              </td>
            </tr>
            <tr v-if="expandedEventID === ev.id">
              <td :colspan="accountId ? 6 : 7" class="bg-gray-50 px-3 py-3 dark:bg-dark-800/40">
                <p v-if="probesError" class="text-xs text-red-600 dark:text-red-400">{{ probesError }}</p>
                <p v-else-if="probesLoading" class="text-xs text-gray-500 dark:text-dark-400">加载中…</p>
                <p v-else-if="probes.length === 0" class="text-xs text-gray-500 dark:text-dark-400">这次验证没有探测记录。</p>
                <table v-else class="min-w-full text-xs">
                  <thead class="text-left text-gray-500 dark:text-dark-400">
                    <tr>
                      <th class="py-1 pr-4">第几份</th>
                      <th class="py-1 pr-4">挑战</th>
                      <th class="py-1 pr-4">数字个数</th>
                      <th class="py-1 pr-4">本份归因</th>
                      <th class="py-1 pr-4">累计概率</th>
                      <th class="py-1 pr-4">计入平均</th>
                      <th class="py-1 pr-4">作废原因</th>
                      <th class="py-1">耗时</th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr v-for="probe in probes" :key="probe.id">
                      <td class="py-1 pr-4">{{ probe.part_index }}</td>
                      <td class="py-1 pr-4 font-mono">{{ probe.challenge_id }}</td>
                      <td class="py-1 pr-4">{{ probe.digit_count }}</td>
                      <td class="py-1 pr-4">{{ probe.part_attribution ?? '—' }}</td>
                      <td class="py-1 pr-4">{{ probe.cum_probability === null ? '—' : probe.cum_probability.toFixed(3) }}</td>
                      <td class="py-1 pr-4">{{ probe.counted_in_average ? '是' : '否' }}</td>
                      <td class="py-1 pr-4">{{ probe.invalid_reason ? reasonText(probe.invalid_reason) : '—' }}</td>
                      <td class="py-1">{{ probe.latency_ms === null ? '—' : `${probe.latency_ms}ms` }}</td>
                    </tr>
                  </tbody>
                </table>
              </td>
            </tr>
          </template>
        </tbody>
      </table>
    </div>

    <div v-if="total > events.length || offset > 0" class="mt-3 flex items-center justify-between text-xs text-gray-500 dark:text-dark-400">
      <span>共 {{ total }} 条，当前 {{ offset + 1 }}–{{ offset + events.length }}</span>
      <span class="flex gap-2">
        <!-- 加载期间禁用翻页：筛选条件已经换了而 offset 还是旧的，那个请求的序号更新，
             反而会把正确的首屏结果覆盖掉。 -->
        <button type="button" class="btn btn-secondary btn-sm" :disabled="loading || offset === 0" @click="load(offset - PAGE_SIZE)">上一页</button>
        <button type="button" class="btn btn-secondary btn-sm" :disabled="loading || offset + events.length >= total" @click="load(offset + PAGE_SIZE)">下一页</button>
      </span>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { adminAPI } from '@/api/admin'
import type { Proxy } from '@/types'
import { extractApiErrorMessage } from '@/utils/apiError'
import codexTicketAPI from './api'
import EventFilterSelect from './EventFilterSelect.vue'
import { reasonText } from './labels'
import type { FingerprintProbe, TicketEvent } from './types'

const props = withDefaults(
  defineProps<{
    /**
     * 只看这个账号的事件。给了它，账号筛选器与账号列都收起来。
     *
     * ⚠️ 名字必须是 `accountId` 而不是 `accountID`：模板上的 `:account-id` 经 Vue 的
     * kebab→camel 规则得到的是 `accountId`（连续大写不会被还原）。写成 `accountID` 时这个
     * prop 永远收不到值，于是悄悄退回默认的 null——**事件表会列出全部账号**，而 vue-tsc
     * 不会报错（多余的 attr 落到 fallthrough，缺失的 prop 有默认值）。
     */
    accountId?: number | null
    /** 账号 id → 名字。缺的显示成 #id。 */
    accountNames?: Record<number, string>
    accountOptions?: { value: string; label: string }[]
    modelOptions?: { value: string; label: string }[]
    /** 调用方已加载过的代理列表。不给则本组件自己拉一次——出口列要把 proxy:<id> 显示成代理名。 */
    proxies?: Proxy[] | null
    title?: string
  }>(),
  {
    accountId: null,
    accountNames: () => ({}),
    accountOptions: () => [],
    modelOptions: () => [],
    proxies: null,
    title: '事件',
  },
)

// 取值与后端的 KongEvent* 常量一一对应，改那边要同步这里——筛选框里给错的名字只会查出空列表。
const EVENT_TYPE_OPTIONS = [
  { value: 'fetch', label: 'fetch 主动取票' },
  { value: 'observe', label: 'observe 被动收票' },
  { value: 'observe_probe', label: 'observe_probe 诊断探测' },
  { value: 'verify', label: 'verify 指纹验证' },
  { value: 'inject_fail', label: 'inject_fail 注入未被接受' },
  { value: 'cooldown', label: 'cooldown 进入冷却' },
  { value: 'egress_invalid', label: 'egress_invalid 出口失效' },
  { value: 'account_unready', label: 'account_unready 账号不可调度' },
  { value: 'probe_skipped', label: 'probe_skipped 未取样' },
  { value: 'fetch_skipped', label: 'fetch_skipped 取票未发出' },
]
const PAGE_SIZE = 50

const events = ref<TicketEvent[]>([])
const total = ref(0)
const offset = ref(0)
const loading = ref(false)
const loadError = ref('')
const accountFilter = ref<string[]>([])
const modelFilter = ref<string[]>([])
const typeFilter = ref<string[]>([])

const expandedEventID = ref<number | null>(null)
const probes = ref<FingerprintProbe[]>([])
const probesLoading = ref(false)
const probesError = ref('')
// 请求序号：只有最新一次请求的结果才写入状态。否则先展开 A 再展开 B、而 A 较晚返回时，
// B 的明细会被 A 的覆盖。
let probesSeq = 0
let eventsSeq = 0

const ownProxies = ref<Proxy[]>([])
const proxyList = computed<Proxy[]>(() => props.proxies ?? ownProxies.value)

// 时间列每秒重算一次：页面常驻过零点时，昨天的行要自己补上日期。
const nowTick = ref(Date.now())
let tickTimer: ReturnType<typeof setInterval> | undefined

async function load(next: number): Promise<void> {
  loading.value = true
  loadError.value = ''
  const want = Math.max(0, next)
  const seq = ++eventsSeq
  // 换了筛选或翻页就关掉已展开的明细：它属于上一份列表。
  expandedEventID.value = null
  probesSeq++
  try {
    const page = await codexTicketAPI.listEvents({
      account_ids: props.accountId ? [props.accountId] : accountFilter.value.map(Number),
      models: modelFilter.value,
      event_types: typeFilter.value,
      limit: PAGE_SIZE,
      offset: want,
    })
    if (seq !== eventsSeq) return
    events.value = page.items ?? []
    total.value = page.total
    offset.value = want
  } catch (error) {
    if (seq !== eventsSeq) return
    loadError.value = extractApiErrorMessage(error, '查询事件失败')
  } finally {
    if (seq === eventsSeq) loading.value = false
  }
}

async function toggleProbes(ev: TicketEvent): Promise<void> {
  if (expandedEventID.value === ev.id) {
    expandedEventID.value = null
    return
  }
  const verificationID = verificationIDOf(ev)
  if (!verificationID) return
  expandedEventID.value = ev.id
  probes.value = []
  probesError.value = ''
  probesLoading.value = true
  const seq = ++probesSeq
  try {
    const items = await codexTicketAPI.listProbes(verificationID)
    if (seq !== probesSeq) return
    probes.value = items
  } catch (error) {
    if (seq !== probesSeq) return
    probesError.value = extractApiErrorMessage(error, '加载探测明细失败')
  } finally {
    if (seq === probesSeq) probesLoading.value = false
  }
}

async function loadProxies(): Promise<void> {
  try {
    // 这是真分页接口，只取第一页。代理是运维级的量（十几个），200 远超实际；**真超了的后果是
    // 第一页之外的代理在出口列显示成 proxy:<id> 原串**，不是报错也不丢信息，所以不为它加翻页
    // 循环。用分页默认的 20 才是会踩到的——那时靠后的代理就查不到名字了。
    const page = await adminAPI.proxies.list(1, 200)
    ownProxies.value = page.items ?? []
  } catch {
    // 拿不到代理只影响出口列的显示——那时 egressLabel 保留 proxy:<id> 原串，不必打扰使用者。
    ownProxies.value = []
  }
}

function accountName(id: number): string {
  return props.accountNames[id] ?? `#${id}`
}

/**
 * egressLabel 把出口键里的 `proxy:<id>` 显示成代理名。
 *
 * 只动这一种形态：`direct` / `none` / `proxy:unset` 是语义值，而 `verify:<账号>`、`observe:<账号>`
 * 是内部任务槽位键，都按原样显示。
 *
 * **查不到对应代理时保留原串**（代理被删了，或列表还没加载完）——换成"未知"会把那个 id 丢掉，而
 * 排查"这条事件走的是哪个代理"恰恰需要它。
 */
function egressLabel(key: string): string {
  const m = /^proxy:(\d+)$/.exec(key ?? '')
  if (!m) return key || '—'
  const proxy = proxyList.value.find((p) => String(p.id) === m[1])
  return proxy ? proxy.name : key
}

// egressTitle 给出口键配一个完整形态的 tooltip：代理名之外还要能看到 id 与连接串。
function egressTitle(key: string): string {
  const m = /^proxy:(\d+)$/.exec(key ?? '')
  if (!m) return key
  const proxy = proxyList.value.find((p) => String(p.id) === m[1])
  return proxy ? `${key} · ${proxy.protocol}://${proxy.host}:${proxy.port}` : key
}

function outcomeClass(outcome: string): string {
  if (outcome === 'success') return 'badge-success'
  if (outcome === 'failure') return 'badge-danger'
  // inconclusive 不用危险色：它对票什么都没说（没测出来），与"测出不合格"是两件事。
  if (outcome === 'inconclusive') return 'badge-warning'
  return 'badge-gray'
}

// stg 是这条事件的结论出自哪一层：0 = 上游自己回报的 model，缺失即 stg1（含全部历史事件）。
// 不按「概率是不是 1」去猜——stg1 的概率也可以恰好是 1，而两者的证据强度完全不同。
function stgOf(ev: TicketEvent): number | null {
  const raw = ev.detail?.stg
  return typeof raw === 'number' ? raw : null
}

// top_models 是归因分布的前三名，由验证事件写进 detail。只看 argmax（fingerprint_model）
// 解释不了拒票：一张真 sol 票可能是 sol 0.82 / 5.5 0.18，看不见第二名就不知道该往白名单里
// 加什么。**历史事件没有这个字段**，取不到时退回只显示 argmax。
function topModelsOf(ev: TicketEvent): { model: string; p: number }[] {
  const raw = ev.detail?.top_models
  if (!Array.isArray(raw)) return []
  const out: { model: string; p: number }[] = []
  for (const item of raw) {
    if (!item || typeof item !== 'object') continue
    const { model, p } = item as { model?: unknown; p?: unknown }
    if (typeof model !== 'string' || !model) continue
    out.push({ model, p: typeof p === 'number' ? p : 0 })
  }
  return out
}

// durationOf 取这条事件对应的上游请求耗时（毫秒），没有则 null。
function durationOf(ev: TicketEvent): number | null {
  const raw = ev.detail?.duration_ms
  return typeof raw === 'number' ? raw : null
}

// 秒是这里的自然刻度：融合取票实测 27–44s，毫秒数只会让人去数位数。
function durationText(ms: number): string {
  return ms < 1000 ? `${ms}ms` : `${(ms / 1000).toFixed(1)}s`
}

/** 事件的 detail 里带 verification_id 时才有探测明细可看。 */
function verificationIDOf(ev: TicketEvent): string {
  const raw = ev.detail?.verification_id
  return typeof raw === 'string' ? raw : ''
}

// top_models 已在前面逐项展开，duration_ms 也已单独显示，这里都去掉以免同一份数据出现两次
// ——top_models 还是 detail 里最长的一项。
function compactDetail(detail: Record<string, unknown>): string {
  const rest: Record<string, unknown> = { ...detail }
  delete rest.top_models
  delete rest.duration_ms
  if (!Object.keys(rest).length) return ''
  const text = JSON.stringify(rest)
  return text.length > 160 ? `${text.slice(0, 160)}…` : text
}

// 当天的时间只显示时刻。事件表里绝大多数行都是今天的，一列重复的年月日会把真正在变的那部分
// 挤到后面去。
function formatTime(value: string): string {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  const today = new Date(nowTick.value)
  const sameDay =
    date.getFullYear() === today.getFullYear() &&
    date.getMonth() === today.getMonth() &&
    date.getDate() === today.getDate()
  return sameDay ? date.toLocaleTimeString() : date.toLocaleString()
}

onMounted(() => {
  void load(0)
  if (!props.proxies) void loadProxies()
  tickTimer = setInterval(() => {
    nowTick.value = Date.now()
  }, 1000)
})

onUnmounted(() => {
  if (tickTimer !== undefined) clearInterval(tickTimer)
})

defineExpose({ reload: () => load(0) })
</script>
