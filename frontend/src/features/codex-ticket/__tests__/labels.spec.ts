import { describe, expect, it } from 'vitest'

import { reasonText, sampleText } from '../labels'

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
