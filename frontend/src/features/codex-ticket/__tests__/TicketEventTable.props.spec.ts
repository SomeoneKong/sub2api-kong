import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const listEvents = vi.fn()
const listProbes = vi.fn()

vi.mock('../api', () => ({
  default: {
    listEvents: (...args: unknown[]) => listEvents(...args),
    listProbes: (...args: unknown[]) => listProbes(...args),
  },
}))

vi.mock('@/api/admin', () => ({
  adminAPI: { proxies: { list: vi.fn().mockResolvedValue({ items: [] }) } },
}))

import TicketEventTable from '../TicketEventTable.vue'

/**
 * 这一组测的不是渲染，而是**prop 名真的能从模板传进去**。
 *
 * 起因：prop 曾叫 `accountID`，模板上写 `:account-id`。Vue 的 kebab→camel 规则给出的是
 * `accountId`（连续大写不会被还原），于是那个 prop 永远收不到值、悄悄退回默认的 null——
 * 单账号明细页的事件表因此列出了全部账号的事件。
 *
 * `vue-tsc` 抓不到这类错误（多余的 attr 落到 fallthrough attrs，缺失的 prop 有默认值），
 * 直接 `mount(Comp, { props: { accountId: 3 } })` 也抓不到（那是 JS 对象、不走模板解析）。
 * 只有**用模板挂载**才会经过 camelize，所以这里刻意走 template 而不是 props。
 */
describe('TicketEventTable 的 account-id', () => {
  beforeEach(() => {
    listEvents.mockReset()
    listEvents.mockResolvedValue({ items: [], total: 0, limit: 50, offset: 0 })
  })

  it('模板上的 :account-id 要能限定查询的账号', async () => {
    mount(
      {
        template: '<TicketEventTable :account-id="3" />',
        components: { TicketEventTable },
      },
      { global: { stubs: { EventFilterSelect: true } } }
    )
    await flushPromises()

    expect(listEvents).toHaveBeenCalledTimes(1)
    expect(listEvents.mock.calls[0][0]).toMatchObject({ account_ids: [3] })
  })

  it('不给 account-id 时不按账号过滤（总览页那份用法）', async () => {
    mount(
      { template: '<TicketEventTable />', components: { TicketEventTable } },
      { global: { stubs: { EventFilterSelect: true } } }
    )
    await flushPromises()

    expect(listEvents).toHaveBeenCalledTimes(1)
    expect(listEvents.mock.calls[0][0]).toMatchObject({ account_ids: [] })
  })

  it('固定账号时收起账号列——那一列全是同一个值', async () => {
    listEvents.mockResolvedValue({
      items: [
        {
          id: 1,
          created_at: new Date().toISOString(),
          account_id: 3,
          model: 'gpt-5.6-sol',
          event_type: 'fetch',
          outcome: 'success',
          status_code: 200,
          traffic_egress: 'direct',
          ticket_egress: 'proxy:100',
          state_len: 292,
          ticket_id: 9,
          fingerprint_model: null,
          idle_seconds: null,
          detail: {},
        },
      ],
      total: 1,
      limit: 50,
      offset: 0,
    })
    const fixed = mount(
      { template: '<TicketEventTable :account-id="3" />', components: { TicketEventTable } },
      { global: { stubs: { EventFilterSelect: true } } }
    )
    await flushPromises()
    expect(fixed.findAll('thead th').map((th) => th.text())).not.toContain('账号')

    const all = mount(
      { template: '<TicketEventTable />', components: { TicketEventTable } },
      { global: { stubs: { EventFilterSelect: true } } }
    )
    await flushPromises()
    expect(all.findAll('thead th').map((th) => th.text())).toContain('账号')
  })
})
