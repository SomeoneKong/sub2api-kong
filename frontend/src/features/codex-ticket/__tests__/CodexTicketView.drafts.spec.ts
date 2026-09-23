import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const getOverview = vi.fn()
const triggerVerify = vi.fn()
const updateAccountConfig = vi.fn()

vi.mock('../api', () => ({
  default: {
    getOverview: (...args: unknown[]) => getOverview(...args),
    triggerVerify: (...args: unknown[]) => triggerVerify(...args),
    updateAccountConfig: (...args: unknown[]) => updateAccountConfig(...args),
  },
}))

vi.mock('@/api/admin', () => ({
  adminAPI: { proxies: { list: vi.fn().mockResolvedValue({ items: [] }) } },
}))

import CodexTicketView from '../CodexTicketView.vue'
import type { TicketAccountStatus } from '../types'

function accountRow(overrides: Partial<TicketAccountStatus> = {}): TicketAccountStatus {
  return {
    account_id: 1,
    name: 'acc-1',
    platform: 'openai',
    ready: true,
    not_ready: '',
    mode: 'full',
    egress: 'direct',
    proxy_id: null,
    config_rejected: null,
    traffic_egress: 'direct',
    ticket_egress: 'direct',
    egress_usable: true,
    egress_reason: '',
    models: [
      {
        model: 'gpt-6-astra',
        current_ticket: null,
        unverified_count: 1,
        diagnosis: null,
        stg0: null,
        last_sample: null,
      },
    ],
    egress_idle_seconds: null,
    next_fetch_allowed_at: null,
    ...overrides,
  }
}

/**
 * 草稿在首次渲染时就按服务端行建好了。单行操作（验票、取票）回写服务端新状态时，没被用户改过的
 * 字段必须跟上新值：否则别处把模式从 full 改成 off 之后，本页下拉仍显示 full，用户只改个出口再保存，
 * 就把模式悄悄写回了 full。
 */
describe('CodexTicketView 的配置草稿', () => {
  beforeEach(() => {
    getOverview.mockReset()
    triggerVerify.mockReset()
    updateAccountConfig.mockReset()
    getOverview.mockResolvedValue({
      accounts: [accountRow()],
      enabled: true,
      gated_models: ['gpt-6-astra'],
      stg0_accept: null,
      fingerprint_accept: null,
      params: {},
      idle_seconds_caveat: '',
    })
  })

  it('回写服务端新状态时，未编辑的字段跟随新值、编辑过的保留', async () => {
    const wrapper = mount(CodexTicketView, {
      global: {
        stubs: {
          AppLayout: { template: '<div><slot /></div>' },
          RouterLink: true,
          TicketEventTable: true,
        },
      },
    })
    await flushPromises()

    const selects = () => wrapper.findAll('select')
    expect((selects()[0].element as HTMLSelectElement).value).toBe('full')
    // 用户只改了出口。
    await selects()[1].setValue('none')

    // 别处已把模式改成 off：验票回来的这一行带着新模式。
    triggerVerify.mockResolvedValue({
      result: { steps: [], not_applicable: true, deny_reason: '' },
      status: accountRow({ mode: 'off' }),
    })
    const verifyButton = wrapper.findAll('button').find((b) => b.text() === '立即验票')
    expect(verifyButton).toBeDefined()
    await verifyButton!.trigger('click')
    await flushPromises()

    expect((selects()[0].element as HTMLSelectElement).value).toBe('off')
    expect((selects()[1].element as HTMLSelectElement).value).toBe('none')

    updateAccountConfig.mockResolvedValue(accountRow({ mode: 'off', egress: 'none' }))
    const saveButton = wrapper.findAll('button').find((b) => b.text() === '保存')
    await saveButton!.trigger('click')
    await flushPromises()
    expect(updateAccountConfig).toHaveBeenCalledWith(1, { mode: 'off', egress: 'none', proxy_id: null })
  })
})
