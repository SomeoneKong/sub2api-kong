<template>
  <AppLayout>
    <div class="mx-auto max-w-[1400px] pb-8">
      <header class="mb-6">
        <RouterLink to="/admin/kong-ticket" class="text-sm text-primary-600 hover:underline dark:text-primary-400">
          ← 返回票据总览
        </RouterLink>
        <h1 class="mt-2 text-2xl font-semibold tracking-tight text-gray-950 dark:text-white">
          票据明细<span v-if="page?.account"> · {{ page.account.name }}</span>
        </h1>
        <p v-if="page?.account" class="mt-2 text-sm text-gray-500 dark:text-dark-300">
          #{{ page.account.account_id }} · 模式 {{ page.account.mode }} · 票据出口
          <span :title="egressTitle(page.account.ticket_egress)">{{ egressLabel(page.account.ticket_egress) }}</span>
          · 流量出口 <span :title="egressTitle(page.account.traffic_egress)">{{ egressLabel(page.account.traffic_egress) }}</span>
          <span v-if="!page.account.ready" class="text-amber-700 dark:text-amber-300">· 诊断暂停：{{ page.account.not_ready }}</span>
        </p>
      </header>

      <div v-if="loadError" role="alert" class="rounded-xl border border-red-200 bg-red-50 p-5 dark:border-red-900 dark:bg-red-950/30">
        <p class="text-sm text-red-700 dark:text-red-300">{{ loadError }}</p>
        <button type="button" class="btn btn-secondary btn-sm mt-3" @click="load">重试</button>
      </div>

      <section v-else class="card px-4 py-4 sm:px-6">
        <div class="mb-3 flex items-center justify-between gap-3">
          <h2 class="text-sm font-semibold text-gray-900 dark:text-white">
            票（{{ tickets.length }} 张<span v-if="page?.truncated">，更早的未列出</span>）
          </h2>
          <button type="button" class="btn btn-secondary btn-sm" :disabled="loading || verifyingID !== null" @click="load">
            刷新
          </button>
        </div>
        <!-- 已过期与已拒的都列出来：这一页要能回答「为什么现在没票可用」，只列可用的等于把答案藏起来。 -->
        <p class="mb-3 text-xs text-gray-500 dark:text-dark-400">
          含已过期与已拒的票。<strong>当前</strong>标记表示此刻业务注入的就是那一张——它由服务端按当前的接受白名单与阈值算出，
          不等于「最新的 verified」。
        </p>

        <p v-if="loading && tickets.length === 0" class="py-6 text-center text-sm text-gray-500 dark:text-dark-400">加载中…</p>
        <p v-else-if="tickets.length === 0" class="py-6 text-center text-sm text-gray-500 dark:text-dark-400">
          这个账号名下没有票。
        </p>
        <div v-else class="overflow-x-auto">
          <table class="min-w-full text-sm">
            <thead class="text-left text-xs uppercase tracking-wider text-gray-500 dark:text-dark-400">
              <tr>
                <th class="px-3 py-2">票</th>
                <th class="px-3 py-2">模型</th>
                <th class="px-3 py-2">状态</th>
                <th class="px-3 py-2">来源 / 长度</th>
                <th class="px-3 py-2">归因</th>
                <th class="px-3 py-2">采集</th>
                <th class="px-3 py-2">剩余</th>
                <th class="px-3 py-2"></th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
              <tr v-for="t in tickets" :key="t.id" class="align-top">
                <td class="px-3 py-3 font-mono text-xs">
                  #{{ t.id }}
                  <span v-if="t.is_current && liveRemaining(t) > 0" class="badge badge-success ml-1">当前</span>
                </td>
                <td class="px-3 py-3 text-xs">{{ t.model }}</td>
                <td class="px-3 py-3">
                  <span class="badge" :class="statusClass(t)">{{ statusText(t) }}</span>
                  <!-- 被排除出候选池是个独立的事实，不在 status 里，但它解释了「为什么这张不会再被选中」。 -->
                  <p v-if="t.skip_until_new" class="mt-1 text-xs text-gray-500 dark:text-dark-400">已排除出候选池</p>
                </td>
                <td class="px-3 py-3 text-xs text-gray-500 dark:text-dark-400">
                  {{ t.source }} · {{ t.state_len }}
                </td>
                <td class="px-3 py-3 text-xs text-gray-500 dark:text-dark-400">
                  <!-- stg0 的结论是上游自己回报的 model，不是一次指纹测量：不显示概率，也不列档位分布。
                       票行里那个 p=1 只是为了让下游统一按概率工作，照它显示会把一句声明说成确定性测量。 -->
                  <template v-if="t.stg === 0 && t.fingerprint_model">
                    上游回报 {{ t.fingerprint_model }}
                    <p class="text-gray-400 dark:text-dark-500">stg0，非指纹归因</p>
                  </template>
                  <template v-else-if="t.fingerprint_model">
                    {{ t.fingerprint_model }} · p={{ t.fingerprint_p.toFixed(2) }}
                    <!-- 列出各模型的概率：一张票判不合格时，这里才看得出它被归到了哪个档位。 -->
                    <p v-if="probsText(t)" class="text-gray-400 dark:text-dark-500">{{ probsText(t) }}</p>
                  </template>
                  <template v-else>未验</template>
                </td>
                <td class="px-3 py-3 text-xs text-gray-500 dark:text-dark-400">{{ formatTime(t.captured_at) }}</td>
                <td class="px-3 py-3 text-xs" :class="liveRemaining(t) > 0 ? 'text-gray-500 dark:text-dark-400' : 'text-gray-400 dark:text-dark-500'">
                  {{ liveRemaining(t) > 0 ? `${liveRemaining(t)}s` : '已过期' }}
                </td>
                <td class="px-3 py-3">
                  <!-- 已拒的也能验：改过接受白名单或阈值之后，同一份证据可能就合格了，服务端会先把它
                       复位成候选再验。已过期的不行——票本身已失效，验它只是白烧额度。 -->
                  <button
                    v-if="t.verifiable && liveRemaining(t) > 0"
                    type="button"
                    class="btn-secondary px-2 py-0.5 text-xs"
                    :disabled="verifyingID !== null || loading"
                    :title="verifyTitle(t)"
                    @click="verify(t)"
                  >
                    {{ verifyingID === t.id ? '验证中…' : t.is_current ? '重验' : '验证' }}
                  </button>
                  <!-- 原因由服务端给：「已过期」与「账号不可调度」是完全不同的两件事，
                       页面自己猜会把后者说成前者。 -->
                  <span v-else class="text-xs text-gray-400 dark:text-dark-500">{{ t.not_verifiable_reason || '不可验' }}</span>
                  <p v-if="notes[t.id]" class="mt-1 text-xs" :class="notes[t.id].ok ? 'text-green-700 dark:text-green-300' : 'text-amber-700 dark:text-amber-300'">
                    {{ notes[t.id].text }}
                  </p>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <!-- 这个账号的事件。票表回答"手上有哪些票"，事件流回答"它们是怎么来的、为什么没成"——
           排查时两者要对着看，跳回总览页再筛一次账号是多余的一步。表与总览页共用同一个组件。 -->
      <TicketEventTable
        v-if="page?.account"
        class="mt-6"
        :account-id="accountID"
        :account-names="{ [accountID]: page.account.name }"
        :model-options="modelOptions"
        :proxies="proxies"
        title="这个账号的事件"
      />
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import { adminAPI } from '@/api/admin'
import AppLayout from '@/components/layout/AppLayout.vue'
import type { Proxy } from '@/types'
import { extractApiErrorMessage } from '@/utils/apiError'
import codexTicketAPI from './api'
import TicketEventTable from './TicketEventTable.vue'
import type { TicketDetailPage, TicketDetailRow } from './types'

const route = useRoute()
const accountID = Number(route.params.id)

const page = ref<TicketDetailPage | null>(null)
const loading = ref(false)
const loadError = ref('')
const verifyingID = ref<number | null>(null)
// 剩余寿命按「响应到达时刻 + 经过时间」本地推算，不直接显示服务端那个快照值：停在页面上不动时，
// 一张过期的票会一直显示成还剩几秒、还带着「当前」标记和验票按钮。
//
// 用经过时间而不是拿 expires_at 跟本地时钟比，是因为浏览器与服务器的时钟未必一致——差几分钟就会
// 把一张好票显示成已过期。总览页用的是同一口径。
const loadedAt = ref(Date.now())
const now = ref(Date.now())
let ticker: ReturnType<typeof setInterval> | null = null
const notes = ref<Record<number, { text: string; ok: boolean }>>({})

const proxies = ref<Proxy[]>([])

const tickets = computed<TicketDetailRow[]>(() => page.value?.tickets ?? [])

/**
 * 事件筛选器的模型选项。
 *
 * **不能只从票推**：取票失败、`fetch_skipped`、312 被拒都产生事件却不产生票，而票还会被过期清理、
 * 本页又有行数上限——正在排查的那个模型很可能一张票都没有，于是从筛选器里消失。所以以**这个账号的
 * 门控模型**为主（`account.models` 就是逐门控模型的状态），再把票里出现过的补进来，后者覆盖"已退出
 * 门控但留着历史票"的情况。
 */
const modelOptions = computed(() => {
  const names = new Set<string>((page.value?.account?.models ?? []).map((m) => m.model))
  for (const t of tickets.value) names.add(t.model)
  return [...names].sort().map((m) => ({ value: m, label: m }))
})

function liveRemaining(t: TicketDetailRow): number {
  const elapsed = Math.floor((now.value - loadedAt.value) / 1000)
  return Math.max(0, t.remaining_seconds - elapsed)
}

async function load(): Promise<void> {
  loading.value = true
  loadError.value = ''
  try {
    page.value = await codexTicketAPI.getTicketDetail(accountID)
    // 每次整页响应都重置基准，否则新响应的剩余秒数会被旧基准的经过时间继续扣。
    loadedAt.value = Date.now()
    now.value = loadedAt.value
  } catch (error) {
    loadError.value = extractApiErrorMessage(error, '加载票据明细失败')
  } finally {
    loading.value = false
  }
}

// 验一张票。服务端会回一份新的详情页——**必须用它整份替换**，不要只改被点的那一行：一次验证会
// 连带改变「当前票是哪张」（验过的票可能顶替旧的，旧票被作废后当前票可能换成别张），只更新一行
// 会让页面上出现两个「当前」或一个都没有。
async function verify(t: TicketDetailRow): Promise<void> {
  verifyingID.value = t.id
  delete notes.value[t.id]
  try {
    const res = await codexTicketAPI.verifyTicket(accountID, t.id)
    const steps = res.result?.steps ?? []
    if (steps.length > 0) {
      const s = steps[0]
      notes.value[t.id] = { text: stepText(s), ok: s.accepted }
    } else if (res.result?.not_applicable) {
      notes.value[t.id] = { text: '这张票已不可验（已过期或状态已变）', ok: false }
    } else {
      notes.value[t.id] = { text: `未能开始验票：${res.result?.deny_reason || '未知原因'}`, ok: false }
    }
    if (res.detail) {
      page.value = res.detail
      loadedAt.value = Date.now()
      now.value = loadedAt.value
    }
  } catch (error) {
    notes.value[t.id] = { text: extractApiErrorMessage(error, '验票失败'), ok: false }
  } finally {
    verifyingID.value = null
  }
}

function stepText(s: { accepted: boolean; revoked: boolean; inconclusive: boolean; reason: string }): string {
  const why = s.reason ? `：${s.reason}` : ''
  if (s.accepted) return '验证通过'
  if (s.revoked) return `未通过，已作废${why}`
  // 与"已作废"必须分开说：这时票还留着，只是这次没测出结论。
  if (s.inconclusive) return `未得出结论，票保留${why}`
  return `未验${why}`
}

function statusText(t: TicketDetailRow): string {
  if (t.remaining_seconds <= 0) return '已过期'
  switch (t.status) {
    case 'verified':
      // 「在用」只在真的会注入时才说。off / observe 不注入，那时最多是"首选"。
      if (t.is_current) return '合格 · 在用'
      return t.preferred ? '合格 · 首选（该模式不注入）' : '合格'
    case 'rejected':
      return '已拒'
    case 'unverified':
      return '待验'
    default:
      return t.status
  }
}

function statusClass(t: TicketDetailRow): string {
  if (t.remaining_seconds <= 0) return 'badge-gray'
  switch (t.status) {
    case 'verified':
      return 'badge-success'
    case 'rejected':
      return 'badge-warning'
    default:
      return 'badge-primary'
  }
}

function verifyTitle(t: TicketDetailRow): string {
  if (t.is_current) {
    return '重验这张正在服务的票：真验出问题才作废，探测失败只报未得出结论、票保留'
  }
  if (t.status === 'rejected') {
    return '先把这张已拒票复位成候选再验——改过接受白名单或阈值之后，同一份证据可能就合格了'
  }
  // 按 status 分：verified 的是备用票（重验），unverified 才是首次验证的候选。
  //
  // **不承诺"成为当前票"**：当前票按剩余寿命最长的合格票选，一张更早过期的验过了也不会接替；
  // 而 off / observe 压根不注入，那两个模式下永远不会有"当前票"。
  if (t.status === 'verified') {
    return '重验这张已验证的备用票：真验出问题才作废，探测失败只报未得出结论、票保留'
  }
  return '验这张候选；合格即进入可用集合'
}

// 只显示排在前面的几个档位：全列出来一行放不下，而看的人要的是「被归到了哪一档」。
function probsText(t: TicketDetailRow): string {
  const probs = t.fingerprint_probs
  if (!probs) return ''
  return Object.entries(probs)
    .sort((a, b) => b[1] - a[1])
    .slice(0, 3)
    .map(([m, p]) => `${m} ${p.toFixed(2)}`)
    .join(' · ')
}

/**
 * egressLabel 把出口键里的 `proxy:<id>` 显示成代理名。裸 id 在这一行毫无信息量——看的人要知道
 * 走的是哪个代理，而不是去代理管理页对号。
 *
 * 只动这一种形态：`direct` / `none` / `proxy:unset` 是语义值，按原样显示。**查不到对应代理时
 * 保留原串**（代理被删了，或列表还没加载完）——换成"未知"会把那个 id 丢掉，而排查"配置指向哪个
 * 代理"恰恰需要它。
 */
function egressLabel(key: string): string {
  const m = /^proxy:(\d+)$/.exec(key ?? '')
  if (!m) return key || '—'
  const proxy = proxies.value.find((pr) => String(pr.id) === m[1])
  return proxy ? proxy.name : key
}

// egressTitle 给出口键配一个完整形态的 tooltip：代理名之外还要能看到 id 与连接串。
function egressTitle(key: string): string {
  const m = /^proxy:(\d+)$/.exec(key ?? '')
  if (!m) return key
  const proxy = proxies.value.find((pr) => String(pr.id) === m[1])
  return proxy ? `${key} · ${proxy.protocol}://${proxy.host}:${proxy.port}` : key
}

async function loadProxies(): Promise<void> {
  try {
    // 代理是运维级的量（十几个），一次取完；用分页默认的 20 会让靠后的代理查不到名字。
    const list = await adminAPI.proxies.list(1, 200)
    proxies.value = list.items ?? []
  } catch {
    // 拿不到代理只影响出口那一行的显示（退回 proxy:<id> 原串），不值得在页面上报错。
    proxies.value = []
  }
}

// 只有日期与今天不同时才带日期：同一天的票挤满日期会把时刻淹掉。
function formatTime(iso: string): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const now = new Date()
  const sameDay =
    d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate()
  const time = d.toLocaleTimeString('zh-CN', { hour12: false })
  return sameDay ? time : `${d.toLocaleDateString('zh-CN')} ${time}`
}

onMounted(() => {
  void load()
  void loadProxies()
  ticker = setInterval(() => {
    now.value = Date.now()
  }, 1000)
})

onUnmounted(() => {
  if (ticker !== null) clearInterval(ticker)
})
</script>
