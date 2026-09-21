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
  precondition_lost: '开始前前提已失效',
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
