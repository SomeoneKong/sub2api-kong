import { describe, expect, it } from 'vitest'

import {
  endReasonText,
  executionLabel,
  formatProbability,
  partValidityText,
  verdictLabel,
} from '../labels'

describe('executionLabel', () => {
  it('三种执行结果各有文案', () => {
    expect(executionLabel('completed')).toEqual({ text: '完成', tone: 'success' })
    expect(executionLabel('cancelled')).toEqual({ text: '已取消', tone: 'neutral' })
    expect(executionLabel('failed')).toEqual({ text: '失败', tone: 'danger' })
  })

  it('不认识的取值原样显示', () => {
    expect(executionLabel('weird').text).toBe('weird')
  })
})

describe('verdictLabel', () => {
  it('执行完成时按 verdict 给出一致 / 不一致 / 判不准', () => {
    expect(verdictLabel({ execution: 'completed', verdict: 'match' })).toEqual({ text: '一致', tone: 'success' })
    expect(verdictLabel({ execution: 'completed', verdict: 'mismatch' })).toEqual({ text: '不一致', tone: 'danger' })
    expect(verdictLabel({ execution: 'completed', verdict: 'inconclusive' })).toEqual({
      text: '判不准',
      tone: 'warning',
    })
  })

  it('执行失败或取消时没有结论，不能显示成「判不准」', () => {
    expect(verdictLabel({ execution: 'failed' })).toBeNull()
    expect(verdictLabel({ execution: 'cancelled' })).toBeNull()
    // 即使带了 verdict，执行没完成也不采信。
    expect(verdictLabel({ execution: 'failed', verdict: 'match' })).toBeNull()
  })

  it('执行完成却缺 verdict 时显示「无结论」', () => {
    expect(verdictLabel({ execution: 'completed' })).toEqual({ text: '无结论', tone: 'neutral' })
  })
})

describe('endReasonText', () => {
  it('每个 end_reason 都有中文说明', () => {
    const reasons = [
      'reported_model_mismatch',
      'fingerprint_confident',
      'parts_disagree',
      'parts_exhausted',
      'client_cancelled',
      'proxy_unavailable',
      'upstream_status',
      'upstream_error',
      'timeout',
    ]
    for (const reason of reasons) {
      const text = endReasonText(reason)
      expect(text).not.toBe(reason)
      expect(text).toMatch(/[一-龥]/)
    }
    expect(endReasonText('reported_model_mismatch')).toBe('上游回报的模型与目标模型对不上')
  })

  it('不认识的取值原样返回', () => {
    expect(endReasonText('something_new')).toBe('something_new')
  })
})

describe('partValidityText', () => {
  it('有效、无效带原因、无效不带原因', () => {
    expect(partValidityText({ valid: true })).toBe('有效')
    expect(partValidityText({ valid: false, invalid_reason: 'insufficient_digits' })).toBe('无效：数字个数不足')
    expect(partValidityText({ valid: false, invalid_reason: 'truncated' })).toBe('无效：正文不完整')
    expect(partValidityText({ valid: false, invalid_reason: 'brand_new' })).toBe('无效：brand_new')
    expect(partValidityText({ valid: false })).toBe('无效')
  })
})

describe('formatProbability', () => {
  it('显示成保留一位小数的百分比', () => {
    expect(formatProbability(0.9312)).toBe('93.1%')
    expect(formatProbability(1)).toBe('100.0%')
    expect(formatProbability(0)).toBe('0.0%')
  })
})
