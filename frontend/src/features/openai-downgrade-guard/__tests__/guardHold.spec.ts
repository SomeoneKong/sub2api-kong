import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'

import DowngradeGuardBadge from '../DowngradeGuardBadge.vue'
import { downgradeGuardHold, dropDowngradeGuardHold, formatGuardTime, guardBadgeText, guardTooltip } from '../guardHold'
import type { Account } from '@/types'

function makeAccount(extra: Record<string, unknown> | undefined): Account {
  return { id: 6, name: 'acc', platform: 'openai', type: 'oauth', extra } as unknown as Account
}

describe('downgradeGuardHold', () => {
  it('没有持有记录或记录没有操作号时返回 null', () => {
    expect(downgradeGuardHold(makeAccount(undefined))).toBeNull()
    expect(downgradeGuardHold(makeAccount({}))).toBeNull()
    expect(downgradeGuardHold(makeAccount({ kong_downgrade_guard_hold: 'x' }))).toBeNull()
    expect(downgradeGuardHold(makeAccount({ kong_downgrade_guard_hold: { stage: 'confirmed' } }))).toBeNull()
  })

  it('读出阶段与时间，未知阶段按待确认', () => {
    const hold = downgradeGuardHold(
      makeAccount({
        kong_downgrade_guard_hold: { op: 'a', stage: 'confirmed', held_at: '2026-10-11T02:15:26Z', last_good_at: '2026-10-11T01:46:37Z' },
      }),
    )
    expect(hold).toEqual({ stage: 'confirmed', heldAt: '2026-10-11T02:15:26Z', lastGoodAt: '2026-10-11T01:46:37Z' })
    expect(downgradeGuardHold(makeAccount({ kong_downgrade_guard_hold: { op: 'a', stage: 'weird' } }))?.stage).toBe('suspect')
  })
})

describe('文案', () => {
  it('两个阶段各有标记', () => {
    expect(guardBadgeText({ stage: 'suspect', heldAt: null, lastGoodAt: null })).toBe('疑似降智')
    expect(guardBadgeText({ stage: 'confirmed', heldAt: null, lastGoodAt: null })).toBe('已确认降智')
  })

  it('时间按本地时区显示月-日 时:分', () => {
    expect(formatGuardTime(new Date(2026, 9, 11, 10, 5).toISOString())).toBe('10-11 10:05')
    expect(formatGuardTime(null)).toBe('未知')
    expect(formatGuardTime('not a time')).toBe('未知')
  })

  it('提示里有停调时间、影响时段与打开方法；没有正常证据时说明起点不确定', () => {
    const fmt = (v: string | null) => v ?? '未知'
    const known = guardTooltip({ stage: 'suspect', heldAt: 'H', lastGoodAt: 'G' }, fmt)
    expect(known).toContain('停调时间：H')
    expect(known).toContain('可能受影响的时段：G 至 H')
    expect(known).toContain('「可调度」开关')
    const unknown = guardTooltip({ stage: 'confirmed', heldAt: 'H', lastGoodAt: null }, fmt)
    expect(unknown).toContain('不确定（按停调前 24 小时算） 至 H')
    expect(unknown).toContain('luna')
  })
})

describe('DowngradeGuardBadge', () => {
  it('只在有持有记录的账号上显示', () => {
    const none = mount(DowngradeGuardBadge, { props: { account: makeAccount({}) } })
    expect(none.find('span').exists()).toBe(false)
    none.unmount()

    const held = mount(DowngradeGuardBadge, {
      props: { account: makeAccount({ kong_downgrade_guard_hold: { op: 'a', stage: 'suspect', held_at: '2026-10-11T02:15:26Z' } }) },
    })
    expect(held.text()).toBe('疑似降智')
    expect(held.find('span').attributes('title')).toContain('正在用指纹测试确认')
    held.unmount()
  })
})

describe('dropDowngradeGuardHold', () => {
  it('只去掉指定账号的持有记录，其余字段与其他账号不变', () => {
    const held = (id: number) =>
      ({ ...makeAccount({ kong_downgrade_guard_hold: { op: 'a', stage: 'confirmed' }, max_sessions: 3 }), id }) as Account
    const plain = { ...makeAccount({ max_sessions: 2 }), id: 9 } as Account
    const list = [held(1), held(2), plain]
    const next = dropDowngradeGuardHold(list, new Set([1, 9]))
    expect(downgradeGuardHold(next[0])).toBeNull()
    expect((next[0].extra as Record<string, unknown>).max_sessions).toBe(3)
    expect(downgradeGuardHold(next[1])).not.toBeNull()
    expect(next[1]).toBe(list[1])
    expect(next[2]).toBe(plain)
  })
})
