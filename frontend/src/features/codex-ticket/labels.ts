// 两个页面共用的文案表。放在这里而不是各自页面内：同一个 reason 在总览页的诊断行、取样行与事件
// 表的作废原因列都会出现，各存一份必然分叉——分叉的表现是同一个键在两处显示成不同的话。

/**
 * 三条展示路径共用这张表：单份挑战的作废原因、验证未得出结论的原因、取样的处置原因。键取自后端
 * 写进 detail 的 reason 与 probe.invalid_reason，缺键会原样显示英文串。
 *
 * 只收会流到这三处的键。取票与出口类事件的 reason（no_state_returned、
 * egress_busy_other_account、upstream_reissued_state）不经这里——它们在事件明细列按原始 JSON 显示。
 */
const reasonTexts: Record<string, string> = {
  // 单份挑战作废（探测明细的「作废原因」列）
  refusal: '拒答',
  truncated: '截断或请求失败',
  insufficient_digits: '数字个数不足',
  non_ascii_digits: '数字不是 ASCII 数字',
  score_failed: '打分失败',
  candidate_not_accepted: '上游未接受这张候选票',
  ticket_expired: '票已过期',
  // 验证未得出结论
  // 三个产生点：挑战发出前、拿到「上游又下发票」之后、以及三份挑战都没有有效回答之后。
  // 后两个都在挑战**已经发出**（额度已经花掉）之后，所以文案必须是阶段中性的。
  precondition_lost: '验证前提已失效',
  no_valid_answer: '没有一份有效回答',
  stale_result: '前提已失效，结论作废',
  probe_persist_failed: '探测证据入库失败',
  stale_after_probe_persist: '证据落库后前提已失效',
  ticket_changed_during_verify: '验证期间票已过期或被撤销',
  // 取样处置（observe 与 probe_skipped 事件）
  duplicate_state: '票与上次相同，无新样本',
  state_len_denylisted: '长度在黑名单内，不验证',
  interval_not_elapsed: '未到探测间隔',
  account_unready: '账号不可调度',
}

export function reasonText(reason: string): string {
  return reasonTexts[reason] ?? reason
}

// 取样处置的 outcome 兜底文案：只在事件没带 reason 时用。成功那一档不带 reason（没什么可解释的），
// 直出 `success` 会让这一行看起来像半成品，而它恰恰是"现在采到的是什么"最常见的档位。
//
// ⚠️ **兜底不得声称发生在哪个阶段。** 取样行读的是四类事件（fetch / fetch_skipped / observe /
// probe_skipped），无 reason 的 `failure` 里有好几种是"票已经拿到了、后续某一步失败"——被动收票之后
// 清跳过标记失败、顺手清理过期票失败都写成 `observe/failure` 且只带 `error`+`phase`。把它说成
// "取票失败"是错误陈述：票明明已经入库。要细分阶段得先把 phase 带进摘要字段，不能靠 outcome 猜。
const SAMPLE_OUTCOME_TEXT: Record<string, string> = {
  success: '采到票',
  failure: '处置失败',
  skipped: '未取样',
}

/** 取样行的正文。有具体 reason 就用它，否则退到 outcome 的中性说法。 */
export function sampleText(n: { outcome: string; reason: string; state_len: number | null }): string {
  const parts: string[] = []
  if (n.reason) parts.push(reasonText(n.reason))
  else parts.push(SAMPLE_OUTCOME_TEXT[n.outcome] ?? n.outcome)
  if (n.state_len !== null) parts.push(`长度 ${n.state_len}`)
  return parts.join('，')
}

// ——— stg0（上游回报的 model）观测的展示口径 ———
//
// 放在这里而不是页面内的两个理由：**判据要能单测**（「什么时候该标红」是这块唯一会产生错误陈述的
// 地方，埋在模板里只能靠肉眼核），以及往后哪个页面显示这块读数都用同一份口径——两处各写一份时，
// 一侧把「白名单已接受的投放」标红、另一侧不标，运维就无法判断到底配没配上。

/** stg0 统计里展示要用到的字段。只声明用到的部分，便于测试构造。 */
export interface Stg0View {
  total: number
  mismatch: number
  mismatch_accepted: number
  mismatch_unaccepted: number
  unknown: number
  top_reported: Array<{ model: string; count: number; accepted: boolean }> | null
}

/**
 * stg0 观测的正文。窗口与标题由页面的块级标题给出，这里只留数字。
 *
 * 四件事必须能分开：
 *   - **无样本**（窗口内没请求，或统计查询失败）——显示成 0% 会被读成「查过了、没问题」；
 *   - **全都没观测到回报值**——比上一种更危险，它看起来像「没被降智」，其实是这项观测没工作；
 *   - **白名单已接受的改投**——上游确实换了模型，但那是我们认可的替代，不需要动作；
 *   - **未接受的不一致**——唯一要人动手的那类，也是唯一该标红的。
 *
 * 头号数字取**未接受**那份：把两份合成一个数时，一次真正的降智会被一大批已接受的投放稀释到看不见。
 * 已接受的量仍然要显示——上游换了模型是事实，抹掉它等于让页面对一次投放切换失声。
 */
export function stg0Line(s: Stg0View | null | undefined): string {
  if (!s || s.total === 0) return '无样本'
  const rate = ((s.mismatch_unaccepted / s.total) * 100).toFixed(2)
  const parts = [`未接受 ${s.mismatch_unaccepted}/${s.total}（${rate}%）`]
  if (s.mismatch_accepted > 0) parts.push(`白名单已接受 ${s.mismatch_accepted}`)
  if (s.unknown > 0) parts.push(`未观测 ${s.unknown}`)
  if (s.unknown === s.total) parts.push('← 全部未观测，这个 0 不代表没被降智')
  return parts.join(' · ')
}

/** stg0 行的配色。**只有未接受的不一致才红**——已接受的改投是正常状态。 */
export function stg0Tone(s: Stg0View | null | undefined): string {
  if (!s || s.total === 0) return 'text-gray-500 dark:text-dark-400'
  // 上游自己声明给了别的模型、而我们没放行：比归因更硬的降智证据。
  if (s.mismatch_unaccepted > 0) return 'text-red-600 dark:text-red-400'
  // 一条都没观测到 = 这项观测没在工作，需要注意但不是降智的定论。
  if (s.unknown === s.total) return 'text-amber-700 dark:text-amber-300'
  return 'text-gray-500 dark:text-dark-400'
}

/**
 * 回报值明细，按是否被白名单接受分组。
 *
 * `top_reported` 可能是 null（Go 的 nil 切片、历史数据、明细查询失败分支）。**模板里一律经它取数组**：
 * 直接读 `.length` 会让零 mismatch 的正常账号打崩整个页面。
 */
export function stg0Reported(
  s: Stg0View | null | undefined,
  accepted: boolean
): Array<{ model: string; count: number }> {
  return (s?.top_reported ?? []).filter((r) => r.accepted === accepted)
}

/** 回报值明细的一行文字，如 `gpt-6-sol ×132`。 */
export function stg0ReportedText(items: Array<{ model: string; count: number }>): string {
  return items.map((r) => `${r.model} ×${r.count}`).join('、')
}

/**
 * 白名单的展示文字（`模型 → 接受值` 逐条）。
 *
 * 空表要明确说「只接受自身」，不能留空白：留空时「没配白名单」与「这块没显示出来」看起来一样，
 * 而页面上那行红字究竟该不该出现，正取决于运维能不能确认白名单当前是什么。
 */
export function acceptLines(table: Record<string, string[]> | null | undefined): string {
  const entries = Object.entries(table ?? {})
    .filter(([, accepted]) => accepted.length > 0)
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
  if (entries.length === 0) return '无——每个门控模型只接受它自己（含快照后缀）'
  return entries.map(([model, accepted]) => `${model} → ${accepted.join('、')}`).join('；')
}
