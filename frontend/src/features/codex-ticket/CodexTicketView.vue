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
          <!-- 白名单同属生效范围：门控集合说的是「保护谁」，这两张表说的是「上游给了别的东西时
               算不算合格」。不显示它们，下面那行 stg0 红字就无法判断是没配上、还是配上了但按别的
               口径在标红。两张表分开列——stg0 读上游自己回报的 model 名，归因那张补偿的是指纹区分度
               不足，互抄值会放过真的降智。 -->
          <dl v-if="gatedModels.length" class="mt-2 space-y-1 text-xs text-gray-500 dark:text-dark-400">
            <div>
              <dt class="inline">stg0 额外接受的上游回报值：</dt>
              <dd class="inline font-mono">{{ acceptLines(overview.stg0_accept) }}</dd>
            </div>
            <div>
              <dt class="inline">指纹归因额外接受：</dt>
              <dd class="inline font-mono">{{ acceptLines(overview.fingerprint_accept) }}</dd>
            </div>
          </dl>
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
                    <!-- stg0（上游回报的 model）近三天的观测。放在账号列而不是票那一列：它统计的是
                         **业务请求**，与某一张票的生命周期无关，是这个账号"有没有在被降智"的读数。
                         逐模型一行——票与结论都是 (账号, 模型) 绑定的，合成一个数就分不清是哪个模型。 -->
                    <div v-if="row.models?.length" class="mt-2">
                      <p class="text-xs text-gray-500 dark:text-dark-400">stg0（近三天）</p>
                      <template v-for="m in row.models" :key="`stg0-${m.model}`">
                        <p class="text-xs" :class="stg0Tone(m.stg0)">
                          {{ m.model }} {{ stg0Line(m.stg0) }}
                        </p>
                        <!-- 回报值原文是这块信息里最可操作的部分：上游投放新模型时照着它往
                             stg0_accept 加一条即可。只给比例的话不知道该加什么。
                             未接受与已接受**分两行**：同色混排时，一条真的降智会被一批已接受的
                             投放淹没，而那恰恰是唯一要人动手的那条。 -->
                        <p
                          v-if="stg0Reported(m.stg0, false).length"
                          class="pl-3 text-xs text-rose-700 dark:text-rose-300"
                        >
                          回报（未接受）：{{ stg0ReportedText(stg0Reported(m.stg0, false)) }}
                        </p>
                        <p
                          v-if="stg0Reported(m.stg0, true).length"
                          class="pl-3 text-xs text-gray-500 dark:text-dark-400"
                        >
                          回报（白名单已接受）：{{ stg0ReportedText(stg0Reported(m.stg0, true)) }}
                        </p>
                      </template>
                    </div>
                    <!-- 下一层：这个账号名下的全部票（含已过期与已拒的），每张都能单独验。 -->
                    <RouterLink
                      :to="`/admin/kong-ticket/accounts/${row.account_id}`"
                      class="mt-1 inline-block text-xs text-primary-600 hover:underline dark:text-primary-400"
                    >
                      查看全部票 →
                    </RouterLink>
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
                    <!-- egress=none 是一个明确的配置选择，不是故障，所以不标成「不可用」，也不必
                         再解释一遍——左边那一列就写着「不设置」。 -->
                    <template v-else-if="row.egress === 'none'">
                      <span class="badge badge-gray">未配置</span>
                      <!-- 但 full + 没有票据出口是真的会持续拒服：这个组合看起来配好了，实际只能等
                           业务响应偶然带回一张好票，系统自己无法恢复。 -->
                      <p v-if="row.mode === 'full'" class="mt-1 text-xs text-red-600 dark:text-red-400">
                        full 但没有票据出口：只能等业务响应偶然带回合格票，无自主恢复能力
                      </p>
                    </template>
                    <span v-else-if="row.egress_usable" class="badge badge-success">可用</span>
                    <template v-else>
                      <span class="badge badge-warning">不可用</span>
                      <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">{{ egressReasonText(row.egress_reason) }}</p>
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
                        <!-- off 不注入，这张票只是上次开着 full 时留下的合格票。不标出来会被读成
                             「有当前票」＝「还在受保护」。 -->
                        <span v-if="row.mode === 'off'" class="text-gray-400">· off 不注入</span>
                      </p>
                      <!-- off 本就不该有服务中的票，缺票不是异常，别用警示色。它仍可能有候选——
                           off 照样收业务响应带回的票，只是不注入、不自动探测。 -->
                      <p v-else-if="row.mode === 'off'" class="text-xs text-gray-500 dark:text-dark-400">无当前票（off 不注入）</p>
                      <p v-else class="text-xs text-amber-700 dark:text-amber-300">无可用当前票</p>
                      <p v-if="m.unverified_count > 0" class="text-xs text-gray-500 dark:text-dark-400">
                        可验候选 {{ m.unverified_count }} 张
                      </p>
                      <div class="mt-1 flex items-center gap-2">
                        <!-- 人工干预：立刻走一遍取票/验票，**跳过静默与冷却**（那两条是自动运行用的
                             保守估计，系统只看得见自己产生的出口活动）。
                             不能主动取票的账号直接不显示这个按钮——摆一个永远点不动的钮，读者还得
                             自己去找为什么。判据与服务端的结构性约束同一套：模式、出口、可调度性。 -->
                        <button
                          v-if="canFetch(row)"
                          type="button"
                          class="btn-secondary px-2 py-0.5 text-xs"
                          :disabled="readOnly || busy || refreshingKey !== null"
                          title="人工干预：立刻取票或验候选，跳过静默与冷却。有成熟候选时优先验它（走流量出口，不动票据出口）"
                          @click="triggerRefresh(row, m.model)"
                        >
                          {{ refreshingKey === refreshKey(row.account_id, m.model) ? '执行中…' : '立即取票验票' }}
                        </button>
                        <!-- 立即验票针对**现有的票**：有当前票就重验它，没有就验最新的那张候选
                             （批量取票之后"有票但未验"是常态）。本端点永不取票，那是上一个按钮的事。
                             验证走流量出口，不消耗票据出口的静默，所以不受 canFetch 那几条约束。
                             模式不参与判定——off 照样收票入库，验它才知道该不该开 full。 -->
                        <button
                          v-if="canVerify(row, m)"
                          type="button"
                          class="btn-secondary px-2 py-0.5 text-xs"
                          :disabled="readOnly || busy || refreshingKey !== null"
                          :title="verifyTitle(row, m)"
                          @click="triggerVerify(row, m.model)"
                        >
                          {{ refreshingKey === refreshKey(row.account_id, m.model) ? '执行中…' : '立即验票' }}
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
                      <!-- 取样状态与结论分开：每张票都被挡在验证之前时（如间隔未满），上面那行会一直
                           停在几天前的旧结论上，只有这一行能看出现在根本没在采样。 -->
                      <p v-if="m.last_sample" class="text-xs text-gray-500 dark:text-dark-400">
                        取样 {{ sampleText(m.last_sample) }} · {{ formatTime(m.last_sample.at) }}
                      </p>
                    </div>
                  </td>
                  <td class="px-3 py-3 text-xs text-gray-600 dark:text-dark-300">
                    <!-- off 从不取票，静默与「下次可取票」对它没有意义，后端也不查。 -->
                    <span v-if="row.mode === 'off'" class="text-gray-500 dark:text-dark-400">—</span>
                    <template v-else>
                      <p>{{ row.egress_idle_seconds === null ? '本系统未用过该出口' : `本系统记录 ${row.egress_idle_seconds}s` }}</p>
                      <p v-if="row.next_fetch_allowed_at" class="text-gray-500 dark:text-dark-400">
                        {{ formatTime(row.next_fetch_allowed_at) }} 起
                      </p>
                    </template>
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

        <!-- 事件。取票、验证、拒服都在这里，排查「为什么拒服」只看这一张表。
             表本身与单账号明细页共用同一个组件，改动只需一处。 -->
        <TicketEventTable
          :account-names="accountNameMap"
          :account-options="accountFilterOptions"
          :model-options="modelFilterOptions"
          :proxies="proxies"
        />
      </template>

      <p v-else class="py-10 text-center text-sm text-gray-500 dark:text-dark-400">加载中…</p>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { RouterLink } from 'vue-router'
import AppLayout from '@/components/layout/AppLayout.vue'
import { extractApiErrorMessage } from '@/utils/apiError'
import { adminAPI } from '@/api/admin'
import type { Proxy } from '@/types'
import codexTicketAPI from './api'
import {
  acceptLines,
  reasonText,
  sampleText,
  stg0Line,
  stg0Reported,
  stg0ReportedText,
  stg0Tone,
} from './labels'
import TicketEventTable from './TicketEventTable.vue'
import type {
  TicketDiagnosis,
  TicketSummary,
  TicketAccountStatus,
  TicketEgress,
  TicketManualVerifyStep,
  TicketMode,
  TicketModelStatus,
  TicketOverview,
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

const accountFilterOptions = computed(() =>
  accounts.value.map((row) => ({ value: String(row.account_id), label: row.name })),
)
const modelFilterOptions = computed(() => gatedModels.value.map((m) => ({ value: m, label: m })))

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
    // stg0 的结论来自上游自己的声明，**没有概率**。显示成 p=1.000 会把它读成"一次恰好很确定的
    // 归因"，而两者的可信度完全不同；显示成 p=0.000 更糟——看起来像归因失败。
    if (d.stg === 0) {
      return `上游回报 ${d.fingerprint_model}（stg0，非指纹归因）`
    }
    return `${d.fingerprint_model}（p=${d.probability.toFixed(3)}）`
  }
  return d.reason ? `未得出结论：${reasonText(d.reason)}` : '未得出结论'
}

function diagnosisClass(d: TicketDiagnosis): string {
  if (d.outcome === 'success') return 'text-gray-500 dark:text-dark-400'
  // 只有"测出不合格"用红色。inconclusive 与其余都归到警示色——需要注意但不是定论。
  if (d.outcome === 'failure') return 'text-red-600 dark:text-red-400'
  return 'text-amber-700 dark:text-amber-300'
}

const accounts = computed(() => overview.value?.accounts ?? [])
// 事件表只拿到 account_id，名字要由这里给——它那一列否则只能显示 #id。
const accountNameMap = computed<Record<number, string>>(() =>
  Object.fromEntries(accounts.value.map((row) => [row.account_id, row.name])),
)
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
    // 逐页取完：靠后的代理取不到就选不到，出口也就配不到它上面。停用与过期的也要取，下拉里按原样显示。
    const first = await adminAPI.proxies.list(1, 200)
    const items = [...(first.items ?? [])]
    for (let p = 2; p <= (first.pages ?? 1); p++) {
      const next = await adminAPI.proxies.list(p, 200)
      items.push(...(next.items ?? []))
    }
    proxies.value = items
  } catch (error) {
    proxies.value = []
    proxiesError.value = extractApiErrorMessage(error, '加载失败')
  }
}

// 「立即取票验票」能不能点。
//
// ⚠️ **判据里刻意不含 `egress_usable`**：那个端点叫 refresh，它不只取票——服务端的决策里
// 「有成熟候选」优先于取票（`HasCandidate → VerifyCandidate`），而候选验证走**流量出口**，压根
// 不要求票据出口可用。按 `egress_usable` 藏按钮会把 `egress=none` + 有候选这种真能干活的局面
// 也一起藏掉，那是无谓地削弱人工干预手段。
//
// 剩下两条是真正的结构性约束，不成立时任何主动请求都不会发出：只有 full 在保护范围内，账号不可
// 调度时一切主动动作都停。既无出口又无候选时服务端会回 deny_reason，页面照实显示。
function canFetch(row: TicketAccountStatus): boolean {
  return row.mode === 'full' && row.ready
}

// 立即验票的判据：**手上有票可验**就行。与它验没验过、与票据出口能不能用、与是哪个模式都无关
// ——验证走流量出口、不注入、不占票据出口的静默，问的只是"这张票对应哪个模型"。
//
// **off 也能验**：那个模式照样收票入库（只是不注入、不自动探测），而它手上那些票的档位正是
// "该不该给这个账号开 full"的判据。批量取票之后「有一张未验候选、但没有当前票」是常态局面——
// 那时正是最需要人工把它验起来的时候，藏掉按钮等于只能干等该模型自己来一个请求。
function canVerify(row: TicketAccountStatus, m: TicketModelStatus): boolean {
  if (!row.ready) return false
  return liveRemaining(m.current_ticket, row.account_id) > 0 || m.unverified_count > 0
}

function verifyTitle(row: TicketAccountStatus, m: TicketModelStatus): string {
  const base = '走流量出口，不占票据出口静默；本端点永不取票。'
  if (liveRemaining(m.current_ticket, row.account_id) > 0) {
    // 只有 full 在注入。off / observe 下这张票是"已验证、但这个模式不用它"，说成"正在服务"
    // 与同一行的「off 不注入」自相矛盾。
    const what = row.mode === 'full' ? '重验正在服务的这张票' : '重验这张已验证的票（该模式不注入它）'
    return `${what}。${base}真验出问题才作废，探测失败只报未得出结论、旧票保留`
  }
  return `验最新的那张待验候选（跳过 min_ticket_age）。${base}合格即进入可用集合`
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
// 服务端把该行的最新状态一起回了，直接替换——重新拉 overview 会冲掉其它行未保存的草稿。
//
// 操作本身已经完成、只是补读失败时 status 为空：该行此刻显示的是旧状态。返回 false 让调用方在提示里
// 说出来，不能让旧结论不声不响地留在页面上。
function applyRowStatus(accountID: number, status: TicketAccountStatus | null): boolean {
  if (!status) return false
  const list = overview.value?.accounts
  if (list) {
    const index = list.findIndex((item) => item.account_id === accountID)
    if (index >= 0) list.splice(index, 1, status)
  }
  rowLoadedAt[accountID] = Date.now()
  return true
}

const staleRowNote = '；状态刷新失败，这一行可能仍是旧状态，请刷新页面'

function noteStaleRow(key: string): void {
  const note = refreshNotes[key]
  if (note) note.text += staleRowNote
}

// 立即验票：一次最多验两张——先正在服务的那一张，它真降档被作废后再验一张最新候选去补。
// 每一步的后果不同（作废在服务的票 vs 把候选排出池子），所以按 steps 逐条讲：只报最后一步会
// 把"当前票已经被作废"这件最要紧的事藏起来。
async function triggerVerify(row: TicketAccountStatus, model: string): Promise<void> {
  const key = refreshKey(row.account_id, model)
  refreshingKey.value = key
  delete refreshNotes[key]
  try {
    const res = await codexTicketAPI.triggerVerify(row.account_id, model)
    const r = res.result
    const steps = r?.steps ?? []
    if (steps.length > 0) {
      const parts = steps.map(verifyStepText)
      if (r?.budget_exhausted) {
        // 说清"还有一张没验"，否则用户以为已经验完了。
        parts.push('余量不足没再验下一张，可以再点一次')
      }
      refreshNotes[key] = { text: parts.join('；'), ok: steps[steps.length - 1].accepted }
    } else if (r?.not_applicable) {
      refreshNotes[key] = { text: '没有票可验（当前票与候选都没有）', ok: false }
    } else {
      refreshNotes[key] = { text: `未能开始验票：${denyReasonText(r?.deny_reason)}`, ok: false }
    }
    if (!applyRowStatus(row.account_id, res.status)) noteStaleRow(key)
  } catch (error) {
    refreshNotes[key] = { text: extractApiErrorMessage(error, '验票失败'), ok: false }
  } finally {
    refreshingKey.value = null
  }
}

function verifyStepText(step: TicketManualVerifyStep): string {
  const what = step.candidate ? `候选 #${step.ticket_id}` : `票 #${step.ticket_id}`
  const why = step.reason ? `：${step.reason}` : ''
  if (step.accepted) {
    // 候选验过只是进入可用集合：当前票按剩余寿命最长的合格票选，一张更早过期的票不会顶替它。
    return step.candidate ? `${what} 验证通过，已进入可用集合` : `${what} 重验通过，仍可用`
  }
  if (step.revoked) {
    return `${what} 未通过，${step.candidate ? '已从候选池排除' : '已作废'}${why}`
  }
  if (step.inconclusive) {
    // 与"已作废"必须分开说：这时票还留着，只是这次没测出结论。
    return `${what} 未得出结论，保留${why}`
  }
  return `${what} 未验${why}`
}

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
    if (!applyRowStatus(row.account_id, res.status)) noteStaleRow(key)
  } catch (error) {
    refreshNotes[key] = { text: extractApiErrorMessage(error, '触发失败'), ok: false }
  } finally {
    refreshingKey.value = null
  }
}

// 键取自后端的 KongEgressReason* 常量，改那边要同步这里——对不上只会退化成显示原始串。
// not_configured 不在表里：egress=none 由上面那一格直接显示成「未配置」，走不到这个映射。
const egressReasons: Record<string, string> = {
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

// 当天的时间只显示时刻。事件表里绝大多数行都是今天的，一列重复的年月日会把真正在变的那部分
// 挤到后面去。
//
// 今天取自 nowTick（每秒一跳）而不是 Date.now()：页面常驻过零点时，昨天的行要自己补上日期。
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
  void loadOverview()
  void loadProxies()
  tickTimer = setInterval(() => {
    nowTick.value = Date.now()
  }, 1000)
})

onUnmounted(() => {
  if (tickTimer !== undefined) clearInterval(tickTimer)
})
</script>
