import { describe, expect, it } from 'vitest'

import {
  acceptLines,
  reasonText,
  sampleText,
  stg0Line,
  stg0Reported,
  stg0Tone,
} from '../labels'
import type { Stg0View } from '../labels'

// 取样行是排查降智时唯一能看出"现在采到的是什么"的地方。它说错事实已经发生过一次（把"票已入库、
// 后续清跳过标记失败"说成"取票失败"），所以这里把口径钉住。
describe('sampleText', () => {
  it('有具体原因时优先显示它', () => {
    expect(sampleText({ outcome: 'skipped', reason: 'state_len_denylisted', state_len: 312 })).toBe(
      '长度在黑名单内，不验证，长度 312'
    )
  })

  it('没有原因时用 outcome 的中性说法，不声称发生在哪个阶段', () => {
    // 无 reason 的 observe/failure 有好几种来源都是"票已经入库了、后续某一步失败"（清跳过标记
    // 失败、顺手清理过期票失败）。说成"取票失败"是错误陈述。
    const text = sampleText({ outcome: 'failure', reason: '', state_len: 292 })
    expect(text).toBe('处置失败，长度 292')
    expect(text).not.toContain('取票')
  })

  it('成功取到票的说法涵盖主动取与被动收', () => {
    expect(sampleText({ outcome: 'success', reason: '', state_len: 292 })).toBe('采到票，长度 292')
  })

  it('没有长度就不显示长度', () => {
    expect(sampleText({ outcome: 'skipped', reason: 'interval_not_elapsed', state_len: null })).toBe(
      '未到探测间隔'
    )
  })

  it('未知 outcome 原样带出，不猜也不吞', () => {
    expect(sampleText({ outcome: 'inconclusive', reason: '', state_len: null })).toBe('inconclusive')
  })
})

describe('reasonText', () => {
  it('表里有就翻译', () => {
    expect(reasonText('candidate_not_accepted')).toBe('上游未接受这张候选票')
  })

  it('表里没有就原样显示，不换成"未知"——原串是排查线索', () => {
    expect(reasonText('some_new_reason')).toBe('some_new_reason')
  })
})
// stg0 这块读数的全部价值在于"该不该动手"。把白名单已接受的投放标红，它就会长期红着、被当成背景噪音
// ——那时一次真正的降智反而看不出来。所以这一组把「什么时候红」钉住。
function stg0(over: Partial<Stg0View> = {}): Stg0View {
  return {
    total: 0,
    mismatch: 0,
    mismatch_accepted: 0,
    mismatch_unaccepted: 0,
    unknown: 0,
    top_reported: [],
    ...over,
  }
}

describe('stg0 观测的展示', () => {
  it('无样本不显示成 0%——那会被读成「查过了、没问题」', () => {
    expect(stg0Line(null)).toBe('无样本')
    expect(stg0Line(stg0({ total: 0 }))).toBe('无样本')
    expect(stg0Tone(null)).not.toContain('red')
  })

  it('全部改投都在白名单内时不标红，但改投的量仍然要显示', () => {
    const s = stg0({
      total: 691,
      mismatch: 132,
      mismatch_accepted: 132,
      mismatch_unaccepted: 0,
      top_reported: [{ model: 'gpt-6-sol', count: 132, accepted: true }],
    })
    expect(stg0Tone(s)).not.toContain('red')
    expect(stg0Line(s)).toContain('白名单已接受 132')
    expect(stg0Line(s)).toContain('未接受 0/691（0.00%）')
  })

  it('有未接受的回报值就标红，比例按未接受那份算', () => {
    const s = stg0({
      total: 200,
      mismatch: 102,
      mismatch_accepted: 100,
      mismatch_unaccepted: 2,
      top_reported: [
        { model: 'gpt-5.5', count: 2, accepted: false },
        { model: 'gpt-6-sol', count: 100, accepted: true },
      ],
    })
    expect(stg0Tone(s)).toContain('red')
    expect(stg0Line(s)).toContain('未接受 2/200（1.00%）')
    // 已接受的那 100 次不能把比例撑大：撑大了就没人分得清 2 次降智与 100 次正常投放。
    expect(stg0Line(s)).not.toContain('51.00%')
  })

  it('一条都没观测到时是琥珀色并明说这个 0 不算数', () => {
    const s = stg0({ total: 40, unknown: 40 })
    expect(stg0Tone(s)).toContain('amber')
    expect(stg0Line(s)).toContain('全部未观测')
  })

  it('回报值按是否被接受分组，null 明细不打崩页面', () => {
    const s = stg0({
      total: 10,
      mismatch: 3,
      mismatch_accepted: 2,
      mismatch_unaccepted: 1,
      top_reported: [
        { model: 'gpt-5.5', count: 1, accepted: false },
        { model: 'gpt-6-sol', count: 2, accepted: true },
      ],
    })
    expect(stg0Reported(s, false).map((r) => r.model)).toEqual(['gpt-5.5'])
    expect(stg0Reported(s, true).map((r) => r.model)).toEqual(['gpt-6-sol'])
    expect(stg0Reported(stg0({ top_reported: null }), false)).toEqual([])
    expect(stg0Reported(null, true)).toEqual([])
  })
})

describe('白名单的生效范围文字', () => {
  it('列出每个模型接受的回报值', () => {
    expect(acceptLines({ 'gpt-5.6-sol': ['gpt-6-sol'], 'gpt-6-astra': [] })).toBe(
      'gpt-5.6-sol → gpt-6-sol'
    )
  })

  it('空表要明说「只接受自身」，不能留空白', () => {
    // 留空时「没配白名单」与「这块没显示出来」看起来一样，而页面上那行红字该不该出现正取决于它。
    expect(acceptLines({})).toContain('只接受它自己')
    expect(acceptLines(null)).toContain('只接受它自己')
  })
})
