<template>
  <AppLayout>
    <div class="mx-auto max-w-[1600px] pb-8">
      <header class="mb-6">
        <h1 class="text-2xl font-semibold tracking-tight text-gray-950 dark:text-white">Codex 票据</h1>
        <p class="mt-2 max-w-3xl text-sm text-gray-500 dark:text-dark-300">
          给受门控的 codex 模型准备经独立出口取得、并用指纹归因验证过的 turn-state 票。
          full 模式下拿不到合格票即拒服——不降级放行。
        </p>
      </header>

      <div v-if="loadError" role="alert" class="rounded-xl border border-red-200 bg-red-50 p-5 dark:border-red-900 dark:bg-red-950/30">
        <p class="text-sm text-red-700 dark:text-red-300">{{ loadError }}</p>
        <button type="button" class="btn btn-secondary btn-sm mt-3" @click="loadOverview">重试</button>
      </div>

      <template v-else-if="overview">
        <!-- 全局开关状态。门控集合为空时整个功能不生效，这一条必须显式说明，
             否则页面看起来配好了、实际什么都没发生。 -->
        <section class="card mb-5 px-4 py-4 sm:px-6">
          <h2 class="text-sm font-semibold text-gray-900 dark:text-white">生效范围</h2>
          <p v-if="gatedModels.length === 0" class="mt-2 text-sm text-amber-700 dark:text-amber-300">
            门控模型集合为空，整个票据功能未生效：配置此时不可编辑，服务端也会拒绝写入。
            要启用请设置服务端环境变量 <code>KONG_TICKET_GATED_MODELS</code>。
          </p>
          <p v-else class="mt-2 text-sm text-gray-600 dark:text-dark-300">
            受保护的上游模型：
            <span v-for="m in gatedModels" :key="m" class="badge badge-primary mr-1">{{ m }}</span>
          </p>
          <dl class="mt-3 grid grid-cols-2 gap-x-6 gap-y-1 text-xs text-gray-500 dark:text-dark-400 sm:grid-cols-3 lg:grid-cols-5">
            <div v-for="p in paramRows" :key="p.label">
              <dt class="inline">{{ p.label }}：</dt>
              <dd class="inline font-mono">{{ p.value }}s</dd>
            </div>
          </dl>
        </section>

        <!-- 账号状态与配置。每行既显示状态也能改配置，省掉一层弹窗。 -->
        <section class="card mb-5 px-4 py-4 sm:px-6">
          <div class="mb-3 flex items-center justify-between gap-3">
            <h2 class="text-sm font-semibold text-gray-900 dark:text-white">账号</h2>
            <button type="button" class="btn btn-secondary btn-sm" :disabled="busy" @click="loadOverview">刷新</button>
          </div>
          <p class="mb-3 text-xs text-gray-500 dark:text-dark-400">空闲时长口径：{{ overview.idle_seconds_caveat }}</p>

          <p v-if="accounts.length === 0" class="py-6 text-center text-sm text-gray-500 dark:text-dark-400">
            没有走 codex 协议的账号。
          </p>
          <div v-else class="overflow-x-auto">
            <table class="min-w-full text-sm">
              <thead class="text-left text-xs uppercase tracking-wider text-gray-500 dark:text-dark-400">
                <tr>
                  <th class="px-3 py-2">账号</th>
                  <th class="px-3 py-2">模式</th>
                  <th class="px-3 py-2">票据出口</th>
                  <th class="px-3 py-2">出口可用性</th>
                  <th class="px-3 py-2">当前票</th>
                  <th class="px-3 py-2">空闲 / 下次可取</th>
                  <th class="px-3 py-2"></th>
                </tr>
              </thead>
              <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
                <tr v-for="row in accounts" :key="row.account_id" class="align-top">
                  <td class="px-3 py-3">
                    <p class="font-medium text-gray-900 dark:text-white">{{ row.name }}</p>
                    <p class="text-xs text-gray-500 dark:text-dark-400">
                      #{{ row.account_id }} · 流量出口
                      <span :title="egressTitle(row.traffic_egress)">{{ egressLabel(row.traffic_egress) }}</span>
                    </p>
                    <p v-if="!row.ready" class="mt-1 text-xs text-amber-700 dark:text-amber-300">诊断暂停：{{ row.not_ready }}</p>
                    <!-- 被拒的配置键必须露出来：静默纠正会让「为什么这个账号不取票」无从排查。 -->
                    <p v-if="row.config_rejected?.length" class="mt-1 text-xs text-red-600 dark:text-red-400">
                      配置被拒：{{ row.config_rejected.join('、') }}（已按保守方向取值）
                    </p>
                  </td>
                  <td class="px-3 py-3">
                    <select v-model="draftOf(row).mode" class="input w-28" :disabled="readOnly || loading">
                      <option value="off">off</option>
                      <option value="observe">observe</option>
                      <option value="full">full</option>
                    </select>
                  </td>
                  <td class="px-3 py-3">
                    <select v-model="draftOf(row).egress" class="input w-40" :disabled="readOnly || loading">
                      <option value="none">none（不主动取票）</option>
                      <option value="direct">direct（服务器本机 IP）</option>
                      <option value="proxy">proxy（指定代理）</option>
                    </select>
                    <!-- 原生 select 而不是 ProxySelector：本页表格外层是 overflow-x-auto，
                         按 CSS 规范另一轴的 visible 会被算成 auto，绝对定位的下拉会被裁切。 -->
                    <select
                      v-if="draftOf(row).egress === 'proxy'"
                      v-model="draftOf(row).proxyIDText"
                      class="input mt-1 w-40"
                      :disabled="readOnly || loading"
                    >
                      <option value="">选择代理…</option>
                      <option v-for="p in proxies" :key="p.id" :value="String(p.id)">
                        {{ proxyLabel(p) }}
                      </option>
                      <!-- 配置指向的代理不在列表里（已删除，或列表没加载成功）时仍要显示出来，
                           否则下拉是空白，看不出原本配的是哪一个。 -->
                      <option v-if="orphanProxyID(row)" :value="orphanProxyID(row)">
                        id {{ orphanProxyID(row) }}（不在代理列表中）
                      </option>
                    </select>
                    <p v-if="proxiesError" class="mt-1 text-xs text-amber-600 dark:text-amber-400">
                      代理列表加载失败：{{ proxiesError }}
                    </p>
                  </td>
                  <td class="px-3 py-3">
                    <span v-if="row.mode === 'off'" class="text-xs text-gray-500 dark:text-dark-400">—</span>
                    <span v-else-if="row.egress_usable" class="badge badge-success">可用</span>
                    <template v-else>
                      <span class="badge badge-warning">不可用</span>
                      <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">{{ egressReasonText(row.egress_reason) }}</p>
                      <!-- full + 没有票据出口 = 只能等业务响应偶然带回一张好票，系统自己无法恢复。
                           这个组合看起来配好了，实际会持续拒服，必须写明。 -->
                      <p v-if="row.mode === 'full' && row.egress === 'none'" class="mt-1 text-xs text-red-600 dark:text-red-400">
                        full 但没有票据出口：只能等业务响应偶然带回合格票，无自主恢复能力
                      </p>
                    </template>
                  </td>
                  <td class="px-3 py-3">
                    <p v-if="!row.models?.length" class="text-xs text-gray-500 dark:text-dark-400">—</p>
                    <div v-for="m in row.models ?? []" :key="m.model" class="mb-2 last:mb-0">
                      <p class="text-xs font-medium text-gray-700 dark:text-dark-200">{{ m.model }}</p>
                      <!-- 剩余时间按 expires_at 本地推算：只信后端快照的话，停在页面上不动时
                           过期票会一直显示成有效的当前票。 -->
                      <p v-if="liveRemaining(m.current_ticket, row.account_id) > 0" class="text-xs text-gray-500 dark:text-dark-400">
                        当前票剩 {{ liveRemaining(m.current_ticket, row.account_id) }}s · 来源 {{ m.current_ticket?.source }}
                      </p>
                      <p v-else class="text-xs text-amber-700 dark:text-amber-300">无可用当前票</p>
                      <p v-if="m.unverified_count > 0" class="text-xs text-gray-500 dark:text-dark-400">
                        候选 {{ m.unverified_count }} 张待验
                      </p>
                      <!-- 人工干预：立刻走一遍取票/验票，**跳过静默与冷却**（那两条是自动运行用的
                           保守估计，系统只看得见自己产生的出口活动）。结构性约束仍然生效。 -->
                      <div class="mt-1 flex items-center gap-2">
                        <button
                          type="button"
                          class="btn-secondary px-2 py-0.5 text-xs"
                          :disabled="readOnly || busy || refreshingKey !== null || row.mode !== 'full'"
                          :title="row.mode !== 'full' ? '只有 full 模式会主动取票' : '人工干预：立刻取票/验票，跳过静默与冷却'"
                          @click="triggerRefresh(row, m.model)"
                        >
                          {{ refreshingKey === refreshKey(row.account_id, m.model) ? '执行中…' : '立即取票验票' }}
                        </button>
                        <span v-if="refreshNote(row.account_id, m.model)" class="text-xs" :class="refreshNoteClass(row.account_id, m.model)">
                          {{ refreshNote(row.account_id, m.model) }}
                        </span>
                      </div>
                      <!-- 诊断与当前票分开显示：旧票还在服务、而最近一次探测已归因为别的档位，
                           正是「该开 full」的信号，混在一起会把它藏起来。 -->
                      <p v-if="m.diagnosis" class="text-xs" :class="diagnosisClass(m.diagnosis)">
                        诊断 {{ diagnosisText(m.diagnosis) }} · {{ formatTime(m.diagnosis.at) }}
                        <span v-if="m.diagnosis.ticket_source">· {{ m.diagnosis.ticket_source }} 票</span>
                        <span v-if="diagnosisStale(m.diagnosis, row.account_id)" class="text-amber-700 dark:text-amber-300">· 已过期，仅历史</span>
                      </p>
                      <p v-else class="text-xs text-gray-500 dark:text-dark-400">诊断：无样本</p>
                      <!-- 取样状态与结论分开：每张票都被 312 挡在验证之前时，上面那行会一直停在
                           几天前的旧结论上，只有这一行能看出现在根本没在采样。 -->
                      <p v-if="m.last_sample" class="text-xs text-gray-500 dark:text-dark-400">
                        取样 {{ sampleText(m.last_sample) }} · {{ formatTime(m.last_sample.at) }}
                      </p>
                    </div>
                  </td>
                  <td class="px-3 py-3 text-xs text-gray-600 dark:text-dark-300">
                    <p>{{ row.egress_idle_seconds === null ? '本系统未用过该出口' : `本系统记录 ${row.egress_idle_seconds}s` }}</p>
                    <p v-if="row.next_fetch_allowed_at" class="text-gray-500 dark:text-dark-400">
                      {{ formatTime(row.next_fetch_allowed_at) }} 起
                    </p>
                  </td>
                  <td class="px-3 py-3">
                    <button
                      type="button"
                      class="btn btn-primary btn-sm"
                      :disabled="!isDirty(row) || busy || readOnly"
                      @click="save(row)"
                    >
                      保存
                    </button>
                    <p v-if="saveErrors[row.account_id]" class="mt-1 max-w-[16rem] text-xs text-red-600 dark:text-red-400">
                      {{ saveErrors[row.account_id] }}
                    </p>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </section>

        <!-- 事件。取票、验证、拒服都在这里，排查「为什么拒服」只看这一张表。 -->
        <section class="card px-4 py-4 sm:px-6">
          <div class="mb-3 flex flex-wrap items-center gap-3">
            <h2 class="text-sm font-semibold text-gray-900 dark:text-white">事件</h2>
            <EventFilterSelect
              v-model="eventAccountFilter"
              class="w-48"
              all-label="全部账号"
              :options="accountFilterOptions"
              @change="loadEvents(0)"
            />
            <EventFilterSelect
              v-model="eventModelFilter"
              class="w-44"
              all-label="全部模型"
              :options="modelFilterOptions"
              @change="loadEvents(0)"
            />
            <EventFilterSelect
              v-model="eventTypeFilter"
              class="w-48"
              all-label="全部事件类型"
              :options="eventTypeOptions"
              @change="loadEvents(0)"
            />
            <button type="button" class="btn btn-secondary btn-sm" :disabled="eventsLoading" @click="loadEvents(0)">查询</button>
          </div>

          <p v-if="eventsError" role="alert" class="py-4 text-sm text-red-600 dark:text-red-400">{{ eventsError }}</p>
          <p v-else-if="events.length === 0" class="py-6 text-center text-sm text-gray-500 dark:text-dark-400">没有事件。</p>
          <div v-else class="overflow-x-auto">
            <table class="min-w-full text-sm">
              <thead class="text-left text-xs uppercase tracking-wider text-gray-500 dark:text-dark-400">
                <tr>
                  <th class="px-3 py-2">时间</th>
                  <th class="px-3 py-2">账号</th>
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
                  <td class="px-3 py-2 text-xs text-gray-600 dark:text-dark-300">{{ accountName(ev.account_id) }}</td>
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
                    <span v-if="topModelsOf(ev).length" class="whitespace-nowrap">
                      归因
                      <span
                        v-for="(tm, i) in topModelsOf(ev)"
                        :key="tm.model"
                        :class="i === 0 ? 'font-medium text-gray-700 dark:text-dark-200' : ''"
                      >{{ i > 0 ? ' · ' : ' ' }}{{ tm.model }} {{ tm.p.toFixed(3) }}</span>
                    </span>
                    <span v-else-if="ev.fingerprint_model">归因 {{ ev.fingerprint_model }} </span>
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
                  <td colspan="7" class="bg-gray-50 px-3 py-3 dark:bg-dark-800/40">
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
                          <td class="py-1 pr-4">{{ probe.invalid_reason ? invalidReasonText(probe.invalid_reason) : '—' }}</td>
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

          <div v-if="eventTotal > events.length || eventOffset > 0" class="mt-3 flex items-center justify-between text-xs text-gray-500 dark:text-dark-400">
            <span>共 {{ eventTotal }} 条，当前 {{ eventOffset + 1 }}–{{ eventOffset + events.length }}</span>
            <span class="flex gap-2">
              <!-- 加载期间禁用翻页：筛选条件已经换了而 offset 还是旧的，那个请求的序号更新，
                   反而会把正确的首屏结果覆盖掉。 -->
              <button type="button" class="btn btn-secondary btn-sm" :disabled="eventsLoading || eventOffset === 0" @click="loadEvents(eventOffset - eventLimit)">上一页</button>
              <button type="button" class="btn btn-secondary btn-sm" :disabled="eventsLoading || eventOffset + events.length >= eventTotal" @click="loadEvents(eventOffset + eventLimit)">下一页</button>
            </span>
          </div>
        </section>
      </template>

      <p v-else class="py-10 text-center text-sm text-gray-500 dark:text-dark-400">加载中…</p>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import AppLayout from '@/components/layout/AppLayout.vue'
import { extractApiErrorMessage } from '@/utils/apiError'
import { adminAPI } from '@/api/admin'
import type { Proxy } from '@/types'
import codexTicketAPI from './api'
import EventFilterSelect from './EventFilterSelect.vue'
import type {
  FingerprintProbe,
  TicketDiagnosis,
  TicketSummary,
  TicketAccountStatus,
  TicketEgress,
  TicketEvent,
  TicketMode,
  TicketOverview,
  TicketSampleNote,
} from './types'

interface ConfigDraft {
  mode: TicketMode
  egress: TicketEgress
  /** 用文本存，空串与 0 才能区分开——0 不是合法代理 id。 */
  proxyIDText: string
}

const overview = ref<TicketOverview | null>(null)
// 代理列表只用于把票据出口的选择做成下拉。它加载失败不该拖垮整页——票据状态与它无关，
// 所以单独记错误，那一格退化成「只剩已配的那一项可选」。
const proxies = ref<Proxy[]>([])
const proxiesError = ref('')
// refreshingKey 非空表示有一次手工触发在执行。全页只允许一次：触发是同步的、且会占用该账号的
// 出口槽，并发点几个只会让后面的都得到「已有在途任务」。
const refreshingKey = ref<string | null>(null)
// 每个 (账号, 模型) 的上次触发结果，成功与拒服都要显示——拒服不是错误，是正常结论。
const refreshNotes = reactive<Record<string, { text: string; ok: boolean }>>({})
const loading = ref(false)
const loadError = ref('')
const savingID = ref<number | null>(null)
// 保存与刷新互斥：两者并发时，较早发出的 GET 可能在保存之后返回，把页面显示恢复成旧配置。
// busy 必须**含手工触发**：它同步执行、最长等 5 分钟，期间若允许保存或整页刷新，较早发出的
// 那一方晚到的响应会把本行覆盖回旧状态（三个入口都是无条件写回该行）。三者互斥是最省的时序保护。
const busy = computed(() => loading.value || savingID.value !== null || refreshingKey.value !== null)
// 让页面每秒重算一次剩余时间，而不去重新拉 overview（那会清掉未保存的草稿）。
const nowTick = ref(Date.now())
// loadedAt 是最近一次拿到 overview 的本地时刻，作为倒计时的默认基准。
const loadedAt = ref(Date.now())
// rowLoadedAt 按行覆盖基准：单行保存会拿回该行的新快照（新的剩余秒数），而整页基准仍是很早以前
// ——用旧基准去减，驻页十分钟后保存会把一张还剩五分钟的票立刻显示成过期。
// 保存一行也不能重置整页基准，那会把其它行的旧快照凭空延长寿命。
const rowLoadedAt = reactive<Record<number, number>>({})

function baselineOf(accountID: number | undefined): number {
  if (accountID !== undefined && rowLoadedAt[accountID] !== undefined) return rowLoadedAt[accountID]
  return loadedAt.value
}
let tickTimer: ReturnType<typeof setInterval> | undefined
const saveErrors = reactive<Record<number, string>>({})
const drafts = reactive<Record<number, ConfigDraft>>({})

const events = ref<TicketEvent[]>([])
const eventsLoading = ref(false)
const eventsError = ref('')
const eventTotal = ref(0)
const eventOffset = ref(0)
const eventLimit = 50
// 三个筛选条件都是多选：空数组即该维度不过滤（与后端一致）。
const eventAccountFilter = ref<string[]>([])
const eventTypeFilter = ref<string[]>([])
const eventModelFilter = ref<string[]>([])

const accountFilterOptions = computed(() =>
  accounts.value.map((row) => ({ value: String(row.account_id), label: row.name })),
)
const modelFilterOptions = computed(() => gatedModels.value.map((m) => ({ value: m, label: m })))

const expandedEventID = ref<number | null>(null)
// 请求序号：只有最新一次请求的结果才写入状态。否则先展开 A 再展开 B、而 A 较晚返回时，
// B 的明细会被 A 的覆盖。
let probesSeq = 0
let eventsSeq = 0
const probes = ref<FingerprintProbe[]>([])
const probesLoading = ref(false)
const probesError = ref('')

// 取值与后端的 KongEvent* 常量一一对应，改那边要同步这里——筛选框里给错的名字只会查出空列表。
const eventTypeOptions = [
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

const invalidReasons: Record<string, string> = {
  refusal: '拒答',
  truncated: '截断或请求失败',
  insufficient_digits: '数字个数不足',
  candidate_not_accepted: '上游未接受这张候选票',
  stale_result: '前提已失效，结论作废',
  ticket_expired: '票已过期',
}

function invalidReasonText(reason: string): string {
  return invalidReasons[reason] ?? reason
}

/** 事件的 detail 里带 verification_id 时才有探测明细可看。 */
function verificationIDOf(ev: TicketEvent): string {
  const raw = ev.detail?.verification_id
  return typeof raw === 'string' ? raw : ''
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

/**
 * 本地推算剩余秒数；过期即为 0。
 *
 * 基准是服务端给的 remaining_seconds 加上「本页加载之后过去了多久」，**不是** expires_at 与
 * Date.now() 之差：浏览器时钟与服务器可以差好几分钟，那样算会把一张还有两分钟的有效票立刻显示成
 * 「无可用当前票」，反向偏差则会凭空延长它的寿命。经过时长用同一个时钟的两次读数之差，不受偏差
 * 影响（壁钟被调整的极端情况下只会偏一次，不会累积）。
 */
function elapsedSinceLoad(accountID?: number): number {
  return Math.max(0, Math.floor((nowTick.value - baselineOf(accountID)) / 1000))
}

function liveRemaining(ticket: TicketSummary | null | undefined, accountID?: number): number {
  if (!ticket) return 0
  return Math.max(0, ticket.remaining_seconds - elapsedSinceLoad(accountID))
}

/** 诊断结论是否只算历史。同样按本地经过时长推进，页面长期驻留时也会自己翻过去。 */
function diagnosisStale(d: TicketDiagnosis, accountID?: number): boolean {
  return d.stale || d.stale_after_seconds - elapsedSinceLoad(accountID) <= 0
}

function diagnosisText(d: TicketDiagnosis): string {
  if (d.fingerprint_model) {
    return `${d.fingerprint_model}（p=${d.probability.toFixed(3)}）`
  }
  return d.reason ? `未得出结论：${invalidReasonText(d.reason)}` : '未得出结论'
}

function sampleText(n: TicketSampleNote): string {
  const parts: string[] = []
  if (n.reason) parts.push(invalidReasonText(n.reason))
  else parts.push(n.outcome)
  if (n.state_len !== null) parts.push(`长度 ${n.state_len}`)
  return parts.join('，')
}

function diagnosisClass(d: TicketDiagnosis): string {
  if (d.outcome === 'success') return 'text-gray-500 dark:text-dark-400'
  if (d.outcome === 'failure') return 'text-red-600 dark:text-red-400'
  return 'text-amber-700 dark:text-amber-300'
}

const accounts = computed(() => overview.value?.accounts ?? [])
const gatedModels = computed(() => overview.value?.gated_models ?? [])
// 未启用时配置只读：此时写入不会有任何效果，却会留下一份「看起来配好了」的状态。
const readOnly = computed(() => overview.value !== null && !overview.value.enabled)

const paramRows = computed(() => {
  const p = overview.value?.params
  if (!p) return []
  return [
    { label: '提前换票', value: p.refresh_before_seconds },
    { label: '取票最小空闲', value: p.ticket_fetch_min_idle_seconds },
    { label: '验证失败冷却', value: p.verify_fail_cooldown_seconds },
    { label: '票最小年龄', value: p.min_ticket_age_seconds },
    { label: 'observe 探测间隔', value: p.observe_probe_interval_seconds },
  ]
})

function draftOf(row: TicketAccountStatus): ConfigDraft {
  let draft = drafts[row.account_id]
  if (!draft) {
    draft = reactive({
      mode: row.mode,
      egress: row.egress,
      proxyIDText: row.proxy_id === null ? '' : String(row.proxy_id),
    })
    drafts[row.account_id] = draft
  }
  return draft
}

function isDirty(row: TicketAccountStatus): boolean {
  const draft = draftOf(row)
  const currentProxy = row.proxy_id === null ? '' : String(row.proxy_id)
  return draft.mode !== row.mode || draft.egress !== row.egress || draft.proxyIDText.trim() !== currentProxy
}

async function save(row: TicketAccountStatus): Promise<void> {
  const draft = draftOf(row)
  delete saveErrors[row.account_id]

  // 非 proxy 出口一律把 id 清成 null：留着残值会让「切回 direct 后仍指向旧代理」这种
  // 读不出来的状态存进去。
  let proxyID: number | null = null
  if (draft.egress === 'proxy') {
    const raw = draft.proxyIDText.trim()
    const parsed = Number(raw)
    if (!raw || !Number.isInteger(parsed) || parsed <= 0) {
      saveErrors[row.account_id] = '代理 ID 必须是正整数'
      return
    }
    proxyID = parsed
  }

  savingID.value = row.account_id
  // 提交用的是一份不可变快照：保存期间用户改了草稿也不影响这次提交的内容，
  // 返回后据它判断草稿是否还是「已提交的那份」。
  const submitted = {
    mode: draft.mode,
    egress: draft.egress,
    proxyIDText: draft.proxyIDText.trim(),
  }
  try {
    const updated = await codexTicketAPI.updateAccountConfig(row.account_id, {
      mode: submitted.mode,
      egress: submitted.egress,
      proxy_id: proxyID,
    })
    // 用服务端回的状态替换本行：保存会顺带改变出口可用性与下次可取时刻，只改本地草稿会
    // 让页面显示一份服务端并不认同的状态。
    const list = overview.value?.accounts
    if (list) {
      const index = list.findIndex((item) => item.account_id === row.account_id)
      if (index >= 0) list.splice(index, 1, updated)
    }
    // 只给这一行换基准：它拿到的是全新的剩余秒数，而其它行的快照还是整页加载时那份。
    rowLoadedAt[row.account_id] = Date.now()
    // 只在草稿仍与本次提交的内容一致时才丢弃它：保存期间用户可能又改了这一行，
    // 无条件删除会把那些编辑悄悄吞掉。
    const current = drafts[row.account_id]
    if (
      current &&
      current.mode === submitted.mode &&
      current.egress === submitted.egress &&
      current.proxyIDText.trim() === submitted.proxyIDText
    ) {
      delete drafts[row.account_id]
    }
  } catch (error) {
    saveErrors[row.account_id] = extractApiErrorMessage(error, '保存失败')
  } finally {
    savingID.value = null
  }
}

async function loadOverview(): Promise<void> {
  loading.value = true
  loadError.value = ''
  try {
    overview.value = await codexTicketAPI.getOverview()
    // 倒计时基准跟着这份快照一起更新：两者必须同源，否则刷新后会拿新剩余秒数减旧的经过时长。
    loadedAt.value = Date.now()
    nowTick.value = loadedAt.value
    // 整页快照是新的，按行覆盖的旧基准全部作废。
    for (const key of Object.keys(rowLoadedAt)) delete rowLoadedAt[Number(key)]
    // 服务端是配置的权威，刷新后丢掉未保存的草稿，避免拿旧草稿覆盖别处的改动。
    // 刷新期间三个配置输入都是禁用的（见模板里的 :disabled），所以这里不会吞掉用户新写的编辑。
    for (const key of Object.keys(drafts)) delete drafts[Number(key)]
  } catch (error) {
    loadError.value = extractApiErrorMessage(error, '加载失败')
  } finally {
    loading.value = false
  }
}

// proxyLabel 与代理管理页同口径：名字 + 连接串；非 active 的标出来但**不过滤掉**——
// 已配置的代理若被停用，隐藏它会让这一行的下拉变空白，反而看不出配的是哪个。
// egressLabel 把出口键里的 `proxy:<id>` 显示成代理名。
//
// 只动这一种形态：`direct` / `none` / `proxy:unset` 是语义值，而 `verify:<账号>`、`observe:<账号>`
// 是内部任务槽位键，都按原样显示。
//
// **查不到对应代理时保留原串**（代理被删了，或列表还没加载完）——换成"未知"会把那个 id 丢掉，而
// 排查"配置指向哪个代理"恰恰需要它。
function egressLabel(key: string): string {
  const m = /^proxy:(\d+)$/.exec(key ?? '')
  if (!m) return key
  const proxy = proxies.value.find((p) => String(p.id) === m[1])
  return proxy ? proxy.name : key
}

// egressTitle 给出口键配一个完整形态的 tooltip：代理名之外还要能看到 id 与连接串。
function egressTitle(key: string): string {
  const m = /^proxy:(\d+)$/.exec(key ?? '')
  if (!m) return key
  const proxy = proxies.value.find((p) => String(p.id) === m[1])
  return proxy ? `${key} · ${proxy.protocol}://${proxy.host}:${proxy.port}` : key
}

function proxyLabel(p: Proxy): string {
  const base = `${p.name}（${p.protocol}://${p.host}:${p.port}）`
  return p.status === 'active' ? base : `${base} [${p.status}]`
}

// orphanProxyID 返回「草稿里配着、但代理列表里没有」的那个 id，没有则返回空串。
function orphanProxyID(row: TicketAccountStatus): string {
  const raw = draftOf(row).proxyIDText.trim()
  if (!raw) return ''
  return proxies.value.some((p) => String(p.id) === raw) ? '' : raw
}

async function loadProxies(): Promise<void> {
  proxiesError.value = ''
  try {
    // 代理是运维级的量（十几个），一次取完；用分页默认的 20 会让靠后的代理选不到。
    const page = await adminAPI.proxies.list(1, 200)
    proxies.value = page.items ?? []
  } catch (error) {
    proxies.value = []
    proxiesError.value = extractApiErrorMessage(error, '加载失败')
  }
}

function refreshKey(accountID: number, model: string): string {
  return `${accountID}::${model}`
}

function refreshNote(accountID: number, model: string): string {
  return refreshNotes[refreshKey(accountID, model)]?.text ?? ''
}

function refreshNoteClass(accountID: number, model: string): string {
  const ok = refreshNotes[refreshKey(accountID, model)]?.ok
  return ok ? 'text-emerald-700 dark:text-emerald-400' : 'text-amber-700 dark:text-amber-300'
}

// triggerRefresh 手工走一遍取票/验票。
//
// 拿到票与被拒都算正常结果，区别只在提示文案；只有请求本身失败（未启用、模型不在门控集合、
// 账号不存在）才是错误。
async function triggerRefresh(row: TicketAccountStatus, model: string): Promise<void> {
  const key = refreshKey(row.account_id, model)
  refreshingKey.value = key
  delete refreshNotes[key]
  try {
    const res = await codexTicketAPI.triggerRefresh(row.account_id, model)
    const r = res.result
    const parts: string[] = []
    if (r?.revived_ticket_id) parts.push(`复用已拒票 #${r.revived_ticket_id} 重验`)
    if (r?.allowed) {
      parts.push(`已拿到票${r.ticket_id ? ` #${r.ticket_id}` : ''}`)
      refreshNotes[key] = { text: parts.join('，'), ok: true }
    } else {
      parts.push(`未拿到票：${denyReasonText(r?.deny_reason)}`)
      if (r?.retry_after) parts.push(`可再试于 ${formatTime(r.retry_after)}`)
      refreshNotes[key] = { text: parts.join('，'), ok: false }
    }
    // 服务端把该行的最新状态一起回了，直接替换——重新拉 overview 会冲掉其它行未保存的草稿。
    if (res.status) {
      const list = overview.value?.accounts
      if (list) {
        const index = list.findIndex((item) => item.account_id === row.account_id)
        if (index >= 0) list.splice(index, 1, res.status)
      }
      rowLoadedAt[row.account_id] = Date.now()
    }
  } catch (error) {
    refreshNotes[key] = { text: extractApiErrorMessage(error, '触发失败'), ok: false }
  } finally {
    refreshingKey.value = null
  }
}

async function loadEvents(offset: number): Promise<void> {
  eventsLoading.value = true
  eventsError.value = ''
  const next = Math.max(0, offset)
  const seq = ++eventsSeq
  // 换了筛选或翻页就关掉已展开的明细：它属于上一份列表。
  expandedEventID.value = null
  probesSeq++
  try {
    const page = await codexTicketAPI.listEvents({
      account_ids: eventAccountFilter.value.map(Number),
      models: eventModelFilter.value,
      event_types: eventTypeFilter.value,
      limit: eventLimit,
      offset: next,
    })
    if (seq !== eventsSeq) return
    events.value = page.items ?? []
    eventTotal.value = page.total
    eventOffset.value = next
  } catch (error) {
    if (seq !== eventsSeq) return
    eventsError.value = extractApiErrorMessage(error, '查询事件失败')
  } finally {
    if (seq === eventsSeq) eventsLoading.value = false
  }
}

function accountName(id: number): string {
  return accounts.value.find((row) => row.account_id === id)?.name ?? `#${id}`
}

// 键取自后端的 KongEgressReason* 常量，改那边要同步这里——对不上只会退化成显示原始串。
const egressReasons: Record<string, string> = {
  not_configured: '未配置票据出口，不会主动取票',
  both_direct: '票据出口与流量出口都是直连，同一个本机 IP',
  same_as_traffic: '票据出口与流量出口是同一个代理',
  proxy_missing: '配置指向的代理不存在（已被删除，或没给代理 ID）',
  proxy_expired: '票据代理已到期',
  proxy_disabled: '票据代理已被停用',
}

function egressReasonText(reason: string): string {
  return egressReasons[reason] ?? reason
}

// 拒服原因的人话。多数不是故障：静默未满、模式不是 full 都是正常结论，文案要说清「等什么」。
const denyReasons: Record<string, string> = {
  account_unready: '账号当前不可调度',
  egress_unusable: '票据出口失效，或与流量出口合并了',
  no_ticket_source: '没配票据出口（egress=none），无法主动取票',
  window_closed: '静默或冷却未满，还不能取票',
  task_no_ticket: '本次取票/验票没有产出可用票——看诊断与事件（上游未下发、长度被挡、或归因不合格）',
  mode_not_full: '该账号不是 full 模式，不参与注入',
  egress_busy: '票据出口正被另一个账号占用',
  wait_timeout: '在途任务未在等待期限内给出结果',
  other_model_task: '在途任务属于另一个模型',
}

function denyReasonText(reason: string | null | undefined): string {
  if (!reason) return '原因未给出'
  return denyReasons[reason] ?? reason
}

function outcomeClass(outcome: string): string {
  if (outcome === 'success') return 'badge-success'
  if (outcome === 'failure') return 'badge-danger'
  if (outcome === 'skipped') return 'badge-warning'
  return 'badge-gray'
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

// top_models 已在前面逐项展开，这里去掉以免同一份数据出现两次——它也是 detail 里最长的一项。
function compactDetail(detail: Record<string, unknown>): string {
  const rest: Record<string, unknown> = { ...detail }
  delete rest.top_models
  if (!Object.keys(rest).length) return ''
  const text = JSON.stringify(rest)
  return text.length > 160 ? `${text.slice(0, 160)}…` : text
}

function formatTime(value: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

onMounted(() => {
  void loadOverview()
  void loadProxies()
  void loadEvents(0)
  tickTimer = setInterval(() => {
    nowTick.value = Date.now()
  }, 1000)
})

onUnmounted(() => {
  if (tickTimer !== undefined) clearInterval(tickTimer)
})
</script>
