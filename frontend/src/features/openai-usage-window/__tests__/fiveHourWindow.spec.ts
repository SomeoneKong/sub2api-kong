import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountUsageCell from '@/components/account/AccountUsageCell.vue'
import type { Account } from '@/types'
import { hidesOpenAIFiveHourWindow } from '../fiveHourWindow'

const { getUsage } = vi.hoisted(() => ({
  getUsage: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getUsage
    }
  }
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

describe('hidesOpenAIFiveHourWindow', () => {
  it.each([
    ['pro', true],
    ['chatgpt_pro', true],
    ['prolite', true],
    [' ProLite ', true],
    ['plus', false],
    ['self_serve_business_prolite', false],
    ['team', false],
    ['', false]
  ])('plan_type %j → %s', (planType, expected) => {
    expect(hidesOpenAIFiveHourWindow({ credentials: { plan_type: planType } })).toBe(expected)
  })

  it('自身 plan_type 为空时按父账号的套餐判断', () => {
    expect(hidesOpenAIFiveHourWindow({ credentials: {}, parent_plan_type: 'pro' })).toBe(true)
    expect(hidesOpenAIFiveHourWindow({ credentials: { plan_type: '  ' }, parent_plan_type: 'prolite' })).toBe(true)
    expect(hidesOpenAIFiveHourWindow({ credentials: { plan_type: 'plus' }, parent_plan_type: 'pro' })).toBe(false)
    expect(hidesOpenAIFiveHourWindow({ credentials: { plan_type: 42 } })).toBe(false)
    expect(hidesOpenAIFiveHourWindow({})).toBe(false)
  })
})

function makeOpenAIAccount(id: number, planType: string): Account {
  return {
    id,
    name: 'account',
    platform: 'openai',
    type: 'oauth',
    credentials: { plan_type: planType },
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: true,
    created_at: '2026-09-23T00:00:00Z',
    updated_at: '2026-09-23T00:00:00Z',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null
  }
}

async function renderUsageBars(account: Account): Promise<string> {
  const windowStats = { requests: 1, tokens: 100, cost: 1, standard_cost: 1, user_cost: 1 }
  getUsage.mockResolvedValue({
    five_hour: { utilization: 0, resets_at: null, remaining_seconds: 0, window_stats: windowStats },
    seven_day: { utilization: 40, resets_at: null, remaining_seconds: 0, window_stats: windowStats }
  })
  const wrapper = mount(AccountUsageCell, {
    props: { account },
    global: {
      stubs: {
        UsageProgressBar: {
          props: ['label', 'utilization'],
          template: '<div class="usage-bar">{{ label }}|{{ utilization }}</div>'
        },
        AccountQuotaInfo: true,
        OpenAIQuotaResetCell: true
      }
    }
  })
  await flushPromises()
  return wrapper.findAll('.usage-bar').map((bar) => bar.text()).join(',')
}

describe('AccountUsageCell 的 OpenAI 5h 窗口', () => {
  it('Pro 20x / Pro 5x 只显示 7d', async () => {
    expect(await renderUsageBars(makeOpenAIAccount(9201, 'pro'))).toBe('7d|40')
    expect(await renderUsageBars(makeOpenAIAccount(9202, 'prolite'))).toBe('7d|40')
  })

  it('其他套餐照常显示 5h 与 7d', async () => {
    expect(await renderUsageBars(makeOpenAIAccount(9203, 'plus'))).toBe('5h|0,7d|40')
  })
})
