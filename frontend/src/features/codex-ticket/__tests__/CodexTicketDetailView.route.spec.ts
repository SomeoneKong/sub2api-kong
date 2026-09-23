import { flushPromises, mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, RouterView } from 'vue-router'

const getTicketDetail = vi.fn()

vi.mock('../api', () => ({
  default: {
    getTicketDetail: (...args: unknown[]) => getTicketDetail(...args),
  },
}))

vi.mock('@/api/admin', () => ({
  adminAPI: { proxies: { list: vi.fn().mockResolvedValue({ items: [] }) } },
}))

import CodexTicketDetailView from '../CodexTicketDetailView.vue'

function detailPage(id: number) {
  return {
    account: { account_id: id, name: `acc-${id}`, mode: 'full', ticket_egress: 'direct', traffic_egress: 'direct', ready: true, not_ready: '', models: [] },
    tickets: [],
    truncated: false,
  }
}

/**
 * 同一路由只换账号参数时，Vue Router 复用组件实例、setup 不重跑。页面必须跟着新参数重新查询，
 * 且上一个账号迟到的响应不能覆盖新页面——否则看到的、验的都还是上一个账号。
 */
describe('CodexTicketDetailView 换账号', () => {
  it('路由参数变化后按新账号查询，旧响应不覆盖新页面', async () => {
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [
        { path: '/admin/kong-ticket', component: { template: '<div />' } },
        { path: '/admin/kong-ticket/accounts/:id', component: CodexTicketDetailView },
      ],
    })
    let resolveFirst: (v: unknown) => void = () => {}
    getTicketDetail.mockImplementation((id: number) =>
      id === 1 ? new Promise((resolve) => { resolveFirst = resolve }) : Promise.resolve(detailPage(id)),
    )

    await router.push('/admin/kong-ticket/accounts/1')
    await router.isReady()
    const wrapper = mount(RouterView, {
      global: {
        plugins: [router],
        stubs: { AppLayout: { template: '<div><slot /></div>' }, TicketEventTable: true },
      },
    })
    await flushPromises()

    await router.push('/admin/kong-ticket/accounts/2')
    await flushPromises()
    expect(getTicketDetail).toHaveBeenLastCalledWith(2)
    expect(wrapper.text()).toContain('acc-2')

    // 账号 1 的响应这时才到：不能把页面换回账号 1。
    resolveFirst(detailPage(1))
    await flushPromises()
    expect(wrapper.text()).toContain('acc-2')
    expect(wrapper.text()).not.toContain('acc-1')
  })
})
