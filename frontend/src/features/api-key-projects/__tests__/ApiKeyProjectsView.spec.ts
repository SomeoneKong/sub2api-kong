import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import type { KongApiKeyProjectStats } from '../api'

const { getMock } = vi.hoisted(() => ({ getMock: vi.fn() }))

vi.mock('@/api/client', () => ({
  apiClient: { get: getMock },
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => (key === 'usage.errors.keyDeleted' ? 'Deleted' : key),
    }),
  }
})

import ApiKeyProjectsView from '../ApiKeyProjectsView.vue'

const stats: KongApiKeyProjectStats = {
  coverage: { start: '2026-10-01 00:00', end: '2026-10-01 03:00' },
  api_keys: [
    { api_key_id: 1, api_key_name: 'small', deleted: true },
    { api_key_id: 2, api_key_name: 'big', deleted: false },
  ],
  rows: [
    {
      api_key_id: 1, model: 'gpt-a', tier: 'standard', bucket: '2026-10-01 01:00',
      requests: 1, input_tokens: 10, output_tokens: 0, cache_creation_tokens: 0, cache_read_tokens: 0, cost: 1, actual_cost: 1,
    },
    {
      api_key_id: 2, model: 'gpt-b', tier: 'standard', bucket: '2026-10-01 01:00',
      requests: 3, input_tokens: 900, output_tokens: 0, cache_creation_tokens: 0, cache_read_tokens: 0, cost: 3, actual_cost: 3,
    },
    {
      api_key_id: 2, model: 'gpt-b', tier: 'fast', bucket: '2026-10-01 02:00',
      requests: 1, input_tokens: 100, output_tokens: 0, cache_creation_tokens: 0, cache_read_tokens: 0, cost: 2, actual_cost: 2,
    },
  ],
}

const mountView = () =>
  mount(ApiKeyProjectsView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        DateRangePicker: {
          name: 'DateRangePicker',
          props: ['startDate', 'endDate'],
          emits: ['change'],
          template: '<div class="range-picker">{{ startDate }}~{{ endDate }}</div>',
        },
        LoadingSpinner: true,
        Icon: true,
        TokenUsageTrend: {
          name: 'TokenUsageTrend',
          props: ['trendData'],
          template: '<div class="trend">{{ JSON.stringify(trendData) }}</div>',
        },
      },
    },
  })

const changeRange = (wrapper: ReturnType<typeof mountView>, startDate: string, endDate: string) =>
  wrapper.findComponent({ name: 'DateRangePicker' }).vm.$emit('change', { startDate, endDate, preset: null })

describe('ApiKeyProjectsView', () => {
  beforeEach(() => {
    getMock.mockReset()
    vi.spyOn(console, 'error').mockImplementation(() => {})
  })

  it('requests the last 7 days and shows loading until the response arrives', async () => {
    let resolve!: (value: { data: KongApiKeyProjectStats }) => void
    getMock.mockReturnValue(new Promise((r) => { resolve = r }))
    const wrapper = mountView()
    await nextTick()

    expect(wrapper.find('[data-testid="loading"]').exists()).toBe(true)
    const [url, config] = getMock.mock.calls[0]
    expect(url).toBe('/usage/dashboard/kong-api-key-projects')
    const days = (new Date(config.params.end_date).getTime() - new Date(config.params.start_date).getTime()) / 86400000
    expect(days).toBe(6)

    resolve({ data: stats })
    await flushPromises()
    expect(wrapper.find('[data-testid="loading"]').exists()).toBe(false)
    expect(wrapper.findAll('[data-testid="key-row"]')).toHaveLength(2)
  })

  it('orders keys by tokens, marks deleted keys and expands the first key by default', async () => {
    getMock.mockResolvedValue({ data: stats })
    const wrapper = mountView()
    await flushPromises()

    const keyRows = wrapper.findAll('[data-testid="key-row"]')
    expect(keyRows[0].text()).toContain('big')
    expect(keyRows[0].text()).toContain('10.0%')
    expect(keyRows[1].text()).toContain('small')
    expect(keyRows[1].text()).toContain('Deleted')
    expect(keyRows[1].text()).toContain('—')
    expect(wrapper.findAll('[data-testid="detail-row"]')).toHaveLength(1)
    expect(wrapper.findAll('[data-testid="tier-row"]').map((row) => row.text().slice(0, 2))).toEqual(['标准', 'Fa'])
  })

  it('does not request again when expanding or switching granularity', async () => {
    getMock.mockResolvedValue({ data: stats })
    const wrapper = mountView()
    await flushPromises()
    expect(getMock).toHaveBeenCalledTimes(1)

    await wrapper.findAll('[data-testid="key-row"]')[1].trigger('click')
    expect(wrapper.findAll('[data-testid="detail-row"]')).toHaveLength(2)

    await wrapper.find('[data-testid="granularity-hour"]').trigger('click')
    const hourly = JSON.parse(wrapper.findAll('.trend')[0].text())
    expect(hourly.map((p: { date: string }) => p.date)).toEqual([
      '2026-10-01 00:00',
      '2026-10-01 01:00',
      '2026-10-01 02:00',
      '2026-10-01 03:00',
    ])

    await wrapper.find('[data-testid="granularity-day"]').trigger('click')
    const daily = JSON.parse(wrapper.findAll('.trend')[0].text())
    expect(daily.map((p: { date: string; total_tokens: number }) => [p.date, p.total_tokens])).toEqual([['2026-10-01', 1000]])

    await wrapper.findAll('[data-testid="key-row"]')[0].trigger('click')
    expect(wrapper.findAll('[data-testid="detail-row"]')).toHaveLength(1)
    expect(getMock).toHaveBeenCalledTimes(1)
  })

  it('shows a failure with retry, distinct from having no data', async () => {
    getMock.mockRejectedValueOnce(new Error('boom'))
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.find('[data-testid="failed"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="empty"]').exists()).toBe(false)

    getMock.mockResolvedValueOnce({ data: stats })
    await wrapper.find('[data-testid="retry"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-testid="failed"]').exists()).toBe(false)
    expect(wrapper.findAll('[data-testid="key-row"]')).toHaveLength(2)
  })

  it('tells apart no usage from dates outside the coverage', async () => {
    getMock.mockResolvedValueOnce({
      data: { coverage: { start: '2026-10-01 00:00', end: '2026-10-07 23:00' }, api_keys: [], rows: [] },
    })
    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.find('[data-testid="empty"]').text()).toBe('可查范围内没有用量')

    getMock.mockResolvedValueOnce({
      data: { coverage: { start: '2026-10-03 19:00', end: '2026-09-01 23:00' }, api_keys: [], rows: [] },
    })
    changeRange(wrapper, '2026-09-01', '2026-09-01')
    await flushPromises()
    expect(wrapper.find('[data-testid="empty"]').text()).toContain('明细只保留 31 天')
  })

  it('hints at the earliest time when the selected start is clamped', async () => {
    getMock.mockResolvedValue({ data: stats })
    const wrapper = mountView()
    changeRange(wrapper, '2026-09-01', '2026-10-01')
    await flushPromises()
    expect(wrapper.find('[data-testid="coverage-hint"]').text()).toContain('2026-10-01 00:00')

    changeRange(wrapper, '2026-10-01', '2026-10-01')
    await flushPromises()
    expect(wrapper.find('[data-testid="coverage-hint"]').exists()).toBe(false)
  })

  it('keeps expanded keys across a range change, without re-applying the default', async () => {
    getMock.mockResolvedValue({ data: stats })
    const wrapper = mountView()
    await flushPromises()

    await wrapper.findAll('[data-testid="key-row"]')[0].trigger('click')
    await wrapper.findAll('[data-testid="key-row"]')[1].trigger('click')
    expect(wrapper.findAll('[data-testid="key-row"]')[1].find('button').attributes('aria-expanded')).toBe('true')

    changeRange(wrapper, '2026-10-01', '2026-10-02')
    await flushPromises()
    expect(wrapper.findAll('[data-testid="key-row"]')[0].find('button').attributes('aria-expanded')).toBe('false')
    expect(wrapper.findAll('[data-testid="key-row"]')[1].find('button').attributes('aria-expanded')).toBe('true')
  })

  it('cancels the previous request on a range change and shows only the latest result', async () => {
    const signals: AbortSignal[] = []
    let rejectFirst!: (reason: unknown) => void
    getMock.mockImplementationOnce((_url: string, config: { signal: AbortSignal }) => {
      signals.push(config.signal)
      return new Promise((_, reject) => { rejectFirst = reject })
    })
    getMock.mockImplementationOnce((_url: string, config: { signal: AbortSignal }) => {
      signals.push(config.signal)
      return Promise.resolve({ data: stats })
    })
    const wrapper = mountView()
    changeRange(wrapper, '2026-10-01', '2026-10-02')
    expect(signals[0].aborted).toBe(true)
    expect(signals[1].aborted).toBe(false)
    await flushPromises()

    rejectFirst(Object.assign(new Error('canceled'), { code: 'ERR_CANCELED' }))
    await flushPromises()
    expect(wrapper.find('[data-testid="failed"]').exists()).toBe(false)
    expect(wrapper.findAll('[data-testid="key-row"]')).toHaveLength(2)
  })

  it('cancels the in-flight request on unmount', () => {
    let signal!: AbortSignal
    getMock.mockImplementationOnce((_url: string, config: { signal: AbortSignal }) => {
      signal = config.signal
      return new Promise(() => {})
    })
    const wrapper = mountView()
    wrapper.unmount()
    expect(signal.aborted).toBe(true)
  })
})
