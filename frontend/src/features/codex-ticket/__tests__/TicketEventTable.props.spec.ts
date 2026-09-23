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

describe('TicketEventTable 的票 id 与探测原因', () => {
  beforeEach(() => {
    listEvents.mockReset()
    listProbes.mockReset()
    listEvents.mockResolvedValue({
      items: [
        {
          id: 1,
          created_at: new Date().toISOString(),
          account_id: 3,
          model: 'gpt-6-astra',
          event_type: 'verify',
          outcome: 'success',
          status_code: null,
          traffic_egress: 'direct',
          ticket_egress: 'direct',
          state_len: 780,
          ticket_id: 91,
          fingerprint_model: null,
          idle_seconds: null,
          detail: { verification_id: 'v-1' },
        },
      ],
      total: 1,
      limit: 50,
      offset: 0,
    })
  })

  // 用量明细写的是「注入票 #7 / 上游回发 #91」，事件行要能用同一个 id 对上号。
  it('事件行显示票 id', async () => {
    const wrapper = mount(
      { template: '<TicketEventTable :account-id="3" />', components: { TicketEventTable } },
      { global: { stubs: { EventFilterSelect: true } } }
    )
    await flushPromises()
    expect(wrapper.text()).toContain('票 #91')
  })

  // 融合样本归因不达标时 invalid_reason 为空、只有 discarded_reason：不显示它，那一行就是
  // "没有原因却没计入平均"。
  it('探测明细分开显示作废原因与弃用原因，并标出融合样本', async () => {
    listProbes.mockResolvedValue([
      {
        id: 10,
        created_at: new Date().toISOString(),
        part_index: 0,
        account_id: 3,
        target_model: 'gpt-6-astra',
        ticket_source: 'fetch',
        verify_egress: 'direct',
        challenge_id: 'c-0',
        digit_count: 200,
        part_attribution: 'gpt-5.6-sol',
        cum_probability: 0.4,
        temperature_tier: 1,
        library_version: null,
        parse_valid: true,
        counted_in_average: false,
        invalid_reason: null,
        fused: true,
        discarded_reason: 'fused_insufficient',
        latency_ms: 30000,
        output_tokens: 900,
      },
    ])
    const wrapper = mount(
      { template: '<TicketEventTable :account-id="3" />', components: { TicketEventTable } },
      { global: { stubs: { EventFilterSelect: true } } }
    )
    await flushPromises()
    const button = wrapper.findAll('button').find((b) => b.text() === '探测明细')
    await button!.trigger('click')
    await flushPromises()
    const probeRow = wrapper.findAll('table table tbody tr').at(-1)!
    expect(probeRow.text()).toContain('融合')
    expect(probeRow.text()).toContain('未采用：融合样本归因未达标')
  })
})
