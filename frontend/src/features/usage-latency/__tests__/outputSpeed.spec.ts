import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import UsageTable from '@/components/admin/usage/UsageTable.vue'
import { estimateOutputTokensPerSecond, formatOutputSpeed } from '../outputSpeed'

vi.mock('@/utils/ipGeoLookup', () => ({
  getEntry: vi.fn(() => ({ status: 'idle' as const })),
  fetchOne: vi.fn(),
  fetchBatch: vi.fn()
}))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }) }))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

describe('estimateOutputTokensPerSecond', () => {
  it('输出 token ÷（总耗时 − 首字）', () => {
    expect(estimateOutputTokensPerSecond({ output_tokens: 500, duration_ms: 6000, first_token_ms: 1000 })).toBe(100)
  })

  it.each([
    ['没有首字时间', { output_tokens: 500, duration_ms: 6000, first_token_ms: null }],
    ['没有总耗时', { output_tokens: 500, duration_ms: null, first_token_ms: 1000 }],
    ['没有输出', { output_tokens: 0, duration_ms: 6000, first_token_ms: 1000 }],
    ['生成时长为零', { output_tokens: 500, duration_ms: 1000, first_token_ms: 1000 }],
    ['首字晚于总耗时', { output_tokens: 500, duration_ms: 900, first_token_ms: 1000 }]
  ])('%s时不给估计', (_, row) => {
    expect(estimateOutputTokensPerSecond(row)).toBeNull()
    expect(formatOutputSpeed(row)).toBeNull()
  })

  it('百以上取整，百以下保留一位小数', () => {
    expect(formatOutputSpeed({ output_tokens: 1234, duration_ms: 11000, first_token_ms: 1000 })).toBe('123 tok/s')
    expect(formatOutputSpeed({ output_tokens: 425, duration_ms: 11000, first_token_ms: 1000 })).toBe('42.5 tok/s')
  })
})

const DataTableStub = {
  props: ['data'],
  template: `
    <div>
      <div v-for="row in data" :key="row.request_id" class="row">
        <slot name="cell-latency" :row="row" />
      </div>
    </div>
  `
}

describe('UsageTable 延迟列的速度估计', () => {
  it('有首字时显示估计值，没有时显示 -', () => {
    const wrapper = mount(UsageTable, {
      props: {
        data: [
          { request_id: 'stream', output_tokens: 500, duration_ms: 6000, first_token_ms: 1000 },
          { request_id: 'sync', output_tokens: 500, duration_ms: 6000, first_token_ms: null }
        ] as any,
        loading: false,
        columns: []
      },
      global: { stubs: { DataTable: DataTableStub, EmptyState: true, Icon: true, Teleport: true } }
    })

    const speeds = wrapper.findAll('[data-testid="usage-output-speed"]').map((cell) => cell.text())
    expect(speeds).toEqual(['100 tok/s', '-'])
    expect(wrapper.text()).toContain('usage.latencyOutputSpeed')
  })
})
