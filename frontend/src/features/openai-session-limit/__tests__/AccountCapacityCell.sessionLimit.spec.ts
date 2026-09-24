import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import AccountCapacityCell from '@/components/account/AccountCapacityCell.vue'
import CapacityBadge from '@/components/account/CapacityBadge.vue'

function mountCell(account: Record<string, unknown>) {
  return mount(AccountCapacityCell, {
    props: { account: { concurrency: 8, current_concurrency: 1, ...account } as any }
  })
}

describe('AccountCapacityCell 的 OpenAI 会话徽标', () => {
  it('设了上限的 OpenAI OAuth 账号在并发下方显示活跃会话数', () => {
    const wrapper = mountCell({ platform: 'openai', type: 'oauth', max_sessions: 3, active_sessions: 2, session_idle_timeout_minutes: 15 })
    const badges = wrapper.findAllComponents(CapacityBadge)
    expect(badges).toHaveLength(2)
    expect(badges[1].props()).toMatchObject({ current: 2, max: 3, tooltip: 'admin.accounts.capacity.sessions.normal' })
  })

  it('满额时用 OpenAI 的提示：新会话优先落到其他账号', () => {
    const wrapper = mountCell({ platform: 'openai', type: 'oauth', max_sessions: 3, active_sessions: 3, session_idle_timeout_minutes: 15 })
    expect(wrapper.findAllComponents(CapacityBadge)[1].props('tooltip')).toBe('admin.accounts.kongSessionLimit.full')
  })

  it('未设上限或非 OAuth 账号不显示', () => {
    expect(mountCell({ platform: 'openai', type: 'oauth' }).findAllComponents(CapacityBadge)).toHaveLength(1)
    expect(mountCell({ platform: 'openai', type: 'apikey', max_sessions: 3 }).findAllComponents(CapacityBadge)).toHaveLength(1)
  })

  it('Anthropic 账号仍用上游提示', () => {
    const wrapper = mountCell({ platform: 'anthropic', type: 'oauth', max_sessions: 2, active_sessions: 2 })
    expect(wrapper.findAllComponents(CapacityBadge)[1].props('tooltip')).toBe('admin.accounts.capacity.sessions.full')
  })
})
