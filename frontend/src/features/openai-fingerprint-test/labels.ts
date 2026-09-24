import type { FingerprintPart, FingerprintResult } from './types'

// 指纹测试结果的中文文案。执行结果与模型结论分开：前者说测试有没有跑完，后者说证据指向什么——
// 执行失败或取消时没有模型结论，不能显示成「判不准」。

export type LabelTone = 'success' | 'danger' | 'warning' | 'neutral'

export interface Label {
  text: string
  tone: LabelTone
}

export function executionLabel(execution: string): Label {
  switch (execution) {
    case 'completed':
      return { text: '完成', tone: 'success' }
    case 'cancelled':
      return { text: '已取消', tone: 'neutral' }
    case 'failed':
      return { text: '失败', tone: 'danger' }
    default:
      return { text: execution, tone: 'neutral' }
  }
}

/** 模型结论；执行没有完成时返回 null（没有结论）。 */
export function verdictLabel(result: Pick<FingerprintResult, 'execution' | 'verdict'>): Label | null {
  if (result.execution !== 'completed') return null
  switch (result.verdict) {
    case 'match':
      return { text: '一致', tone: 'success' }
    case 'mismatch':
      return { text: '不一致', tone: 'danger' }
    case 'inconclusive':
      return { text: '判不准', tone: 'warning' }
    default:
      return { text: result.verdict || '无结论', tone: 'neutral' }
  }
}

const END_REASON_TEXT: Record<string, string> = {
  reported_model_mismatch: '上游回报的模型与目标模型对不上',
  fingerprint_confident: '累计指纹归因达到 0.9，且各份归因一致',
  parts_disagree: '累计归因已达到 0.9，但各份的归因不一致（各次请求可能被路由到不同模型）',
  parts_exhausted: '挑战份数用完，累计归因仍不够下结论',
  client_cancelled: '测试被取消（弹窗关闭或连接断开）',
  proxy_unavailable: '账号的代理不可用',
  upstream_status: '上游返回了错误状态码',
  upstream_error: '请求上游失败',
  timeout: '挑战请求超时',
}

/** end_reason 的中文说明；不认识的取值原样返回。 */
export function endReasonText(reason: string): string {
  return END_REASON_TEXT[reason] ?? reason
}

const INVALID_REASON_TEXT: Record<string, string> = {
  truncated: '正文不完整',
  request_failed: '请求失败',
  insufficient_digits: '数字个数不足',
  non_ascii_digits: '含非 ASCII 数字',
  score_failed: '打分失败',
}

/** 一份挑战是否计入归因；不计入时带上原因。 */
export function partValidityText(part: Pick<FingerprintPart, 'valid' | 'invalid_reason'>): string {
  if (part.valid) return '有效'
  if (!part.invalid_reason) return '无效'
  return `无效：${INVALID_REASON_TEXT[part.invalid_reason] ?? part.invalid_reason}`
}

/** 概率显示成百分比，保留一位小数。 */
export function formatProbability(p: number): string {
  return `${(p * 100).toFixed(1)}%`
}
