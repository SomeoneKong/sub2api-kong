import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

const { getMock } = vi.hoisted(() => ({ getMock: vi.fn() }))

vi.mock('@/api/client', () => ({
  apiClient: { get: getMock },
}))

const messages: Record<string, string> = {
  'usage.apiKeyDistribution': 'API Key Distribution',
  'usage.apiKeyFilter': 'API Key',
  'usage.errors.keyDeleted': 'Deleted',
  'admin.dashboard.requests': 'Requests',
  'admin.dashboard.tokens': 'Tokens',
  'admin.dashboard.actual': 'Actual',
  'admin.dashboard.standard': 'Standard',
  'admin.dashboard.metricTokens': 'By Tokens',
  'admin.dashboard.metricActualCost': 'By Actual Cost',
  'admin.dashboard.noDataAvailable': 'No data available',
}

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => messages[key] ?? key,
    }),
  }
})

vi.mock('vue-chartjs', () => ({
  Doughnut: {
    props: ['data'],
    template: '<div class="chart-data">{{ JSON.stringify(data) }}</div>',
  },
}))

import ApiKeyDistributionChart from '../ApiKeyDistributionChart.vue'
import { useKongApiKeyStats, type KongApiKeyStat } from '../api'

const stats: KongApiKeyStat[] = [
  { api_key_id: 1, api_key_name: 'laptop', deleted: false, requests: 9, total_tokens: 1200, cost: 1.8, actual_cost: 0.1 },
  { api_key_id: 2, api_key_name: 'server', deleted: true, requests: 4, total_tokens: 600, cost: 0.7, actual_cost: 0.9 },
  { api_key_id: 3, api_key_name: '', deleted: false, requests: 1, total_tokens: 30, cost: 0.01, actual_cost: 0.01 },
]

const mountChart = (props: { stats: KongApiKeyStat[]; loading?: boolean }) =>
  mount(ApiKeyDistributionChart, {
    props,
    global: { stubs: { LoadingSpinner: true } },
  })

describe('ApiKeyDistributionChart', () => {
  it('orders by tokens by default, labels deleted and unnamed keys', () => {
    const wrapper = mountChart({ stats })

    const chartData = JSON.parse(wrapper.find('.chart-data').text())
    expect(chartData.labels).toEqual(['laptop', 'server (Deleted)', '#3'])
    expect(chartData.datasets[0].data).toEqual([1200, 600, 30])

    const rows = wrapper.findAll('tbody tr')
    expect(rows).toHaveLength(3)
    expect(rows[0].text()).toContain('laptop')
    expect(rows[0].text()).not.toContain('Deleted')
    expect(rows[1].text()).toContain('server')
    expect(rows[1].text()).toContain('Deleted')
    expect(rows[2].text()).toContain('#3')

    const options = (wrapper.vm as any).$?.setupState.doughnutOptions
    const label = options.plugins.tooltip.callbacks.label({
      label: 'laptop',
      raw: 1200,
      dataset: { data: [1200, 600] },
    })
    expect(label).toBe('laptop: 1.20K (66.7%)')
  })

  it('switches to actual cost and reorders', async () => {
    const wrapper = mountChart({ stats })

    await wrapper.findAll('button')[1].trigger('click')

    const chartData = JSON.parse(wrapper.find('.chart-data').text())
    expect(chartData.labels).toEqual(['server (Deleted)', 'laptop', '#3'])
    expect(chartData.datasets[0].data).toEqual([0.9, 0.1, 0.01])

    const options = (wrapper.vm as any).$?.setupState.doughnutOptions
    const label = options.plugins.tooltip.callbacks.label({
      label: 'server (Deleted)',
      raw: 0.9,
      dataset: { data: [0.9, 0.1] },
    })
    expect(label).toBe('server (Deleted): $0.900 (90.0%)')
  })

  it('cycles colors when there are more keys than the palette', () => {
    const many = Array.from({ length: 12 }, (_, i) => ({
      api_key_id: i + 1,
      api_key_name: `k${i + 1}`,
      deleted: false,
      requests: 1,
      total_tokens: 100 - i,
      cost: 0,
      actual_cost: 0,
    }))
    const wrapper = mountChart({ stats: many })

    const colors = JSON.parse(wrapper.find('.chart-data').text()).datasets[0].backgroundColor
    expect(colors).toHaveLength(12)
    expect(colors[10]).toBe(colors[0])
    expect(colors[11]).toBe(colors[1])
  })

  it('shows the empty state without stats', () => {
    const wrapper = mountChart({ stats: [] })
    expect(wrapper.find('.chart-data').exists()).toBe(false)
    expect(wrapper.text()).toContain('No data available')
  })
})

describe('useKongApiKeyStats', () => {
  it('keeps only the latest response when requests overlap', async () => {
    let resolveFirst!: (value: unknown) => void
    getMock
      .mockImplementationOnce(() => new Promise((resolve) => { resolveFirst = resolve }))
      .mockImplementationOnce(() => Promise.resolve({ data: { api_keys: [stats[1]] } }))

    const { stats: loaded, loading, load } = useKongApiKeyStats()
    const first = load({ start_date: '2026-10-01', end_date: '2026-10-01' })
    const second = load({ start_date: '2026-10-02', end_date: '2026-10-02' })
    await second
    expect(loaded.value).toEqual([stats[1]])
    expect(loading.value).toBe(false)

    resolveFirst({ data: { api_keys: [stats[0]] } })
    await first
    expect(loaded.value).toEqual([stats[1]])
    expect(loading.value).toBe(false)

    expect(getMock).toHaveBeenLastCalledWith('/usage/dashboard/kong-api-key-stats', {
      params: { start_date: '2026-10-02', end_date: '2026-10-02' },
    })
  })
})
