import { describe, expect, it } from 'vitest'
import {
  buildProjectAxis,
  buildProjectTrend,
  clampedCoverageStart,
  defaultProjectGranularity,
  formatFastShare,
  formatProjectShare,
  formatTopModels,
  isOutsideCoverage,
  summarizeProjectKeys,
} from '../aggregate'
import type { KongApiKeyProjectRow, KongApiKeyProjectStats, KongApiKeyProjectTier } from '../api'

const row = (
  api_key_id: number,
  model: string,
  tier: KongApiKeyProjectTier,
  bucket: string,
  tokens: number
): KongApiKeyProjectRow => ({
  api_key_id,
  model,
  tier,
  bucket,
  requests: 1,
  input_tokens: tokens,
  output_tokens: 0,
  cache_creation_tokens: 0,
  cache_read_tokens: 0,
  cost: 0.5,
  actual_cost: 0.25,
})

const statsOf = (
  rows: KongApiKeyProjectRow[],
  coverage = { start: '2026-10-01 00:00', end: '2026-10-01 23:00' }
): KongApiKeyProjectStats => ({
  coverage,
  api_keys: [...new Set(rows.map((r) => r.api_key_id))].map((api_key_id) => ({
    api_key_id,
    api_key_name: `key-${api_key_id}`,
    deleted: false,
  })),
  rows,
})

describe('summarizeProjectKeys', () => {
  it('orders keys by tokens, then id', () => {
    const stats = statsOf([
      row(2, 'm', 'standard', '2026-10-01 01:00', 100),
      row(3, 'm', 'standard', '2026-10-01 01:00', 500),
      row(1, 'm', 'standard', '2026-10-01 01:00', 100),
    ])
    expect(summarizeProjectKeys(stats).map((s) => s.api_key_id)).toEqual([3, 1, 2])
  })

  it('sums all token kinds, requests and costs across rows', () => {
    const summary = summarizeProjectKeys({
      coverage: { start: '2026-10-01 00:00', end: '2026-10-01 23:00' },
      api_keys: [{ api_key_id: 1, api_key_name: 'k', deleted: false }],
      rows: [
        { ...row(1, 'm', 'standard', '2026-10-01 01:00', 1), output_tokens: 2, cache_creation_tokens: 3, cache_read_tokens: 4 },
        { ...row(1, 'm', 'fast', '2026-10-01 02:00', 10), output_tokens: 20, cache_creation_tokens: 30, cache_read_tokens: 40 },
      ],
    })[0]
    expect(summary.total_tokens).toBe(110)
    expect(summary.requests).toBe(2)
    expect(summary.cost).toBeCloseTo(1)
    expect(summary.actual_cost).toBeCloseTo(0.5)
    expect(summary.models[0].total_tokens).toBe(110)
    expect(summary.models[0].tiers.map((t) => t.total_tokens)).toEqual([10, 100])
  })

  it('orders models by tokens and tiers in a fixed order, with shares of the key total', () => {
    const summary = summarizeProjectKeys(statsOf([
      row(1, 'small', 'standard', '2026-10-01 01:00', 100),
      row(1, 'big', 'flex', '2026-10-01 01:00', 100),
      row(1, 'big', 'ultrafast', '2026-10-01 01:00', 100),
      row(1, 'big', 'fast', '2026-10-01 01:00', 100),
      row(1, 'big', 'standard', '2026-10-01 01:00', 600),
    ]))[0]
    expect(summary.models.map((m) => m.model)).toEqual(['big', 'small'])
    const big = summary.models[0]
    expect(big.share).toBeCloseTo(0.9)
    expect(big.tiers.map((t) => t.tier)).toEqual(['standard', 'fast', 'ultrafast', 'flex'])
    expect(big.tiers[1].share).toBeCloseTo(0.1)
  })

  it('lists tier sub-rows only when a model has a non-standard tier', () => {
    const summary = summarizeProjectKeys(statsOf([
      row(1, 'plain', 'standard', '2026-10-01 01:00', 100),
      row(1, 'only-fast', 'fast', '2026-10-01 01:00', 100),
    ]))[0]
    const byModel = Object.fromEntries(summary.models.map((m) => [m.model, m.showTiers]))
    expect(byModel).toEqual({ plain: false, 'only-fast': true })
  })

  it('counts fast and ultrafast but not flex in the fast share', () => {
    const summary = summarizeProjectKeys(statsOf([
      row(1, 'm', 'standard', '2026-10-01 01:00', 700),
      row(1, 'm', 'flex', '2026-10-01 01:00', 100),
      row(1, 'm', 'fast', '2026-10-01 01:00', 100),
      row(1, 'n', 'ultrafast', '2026-10-01 01:00', 100),
    ]))[0]
    expect(summary.fastShare).toBeCloseTo(0.2)
  })

  it('reports null shares when a key only has zero-token records', () => {
    const summary = summarizeProjectKeys(statsOf([row(1, 'search', 'standard', '2026-10-01 01:00', 0)]))[0]
    expect(summary.fastShare).toBeNull()
    expect(summary.models[0].share).toBeNull()
    expect(formatProjectShare(summary.models[0].share)).toBe('—')
    expect(formatFastShare(summary.fastShare)).toBe('—')
    expect(formatTopModels(summary.models)).toBe('search')
  })

  it('labels unnamed and deleted keys', () => {
    const summary = summarizeProjectKeys({
      coverage: { start: '2026-10-01 00:00', end: '2026-10-01 23:00' },
      api_keys: [{ api_key_id: 9, api_key_name: '', deleted: true }],
      rows: [row(9, 'm', 'standard', '2026-10-01 01:00', 1)],
    })[0]
    expect(summary.name).toBe('#9')
    expect(summary.deleted).toBe(true)
  })
})

describe('share formatting', () => {
  it('keeps tiny non-zero shares visible', () => {
    expect(formatProjectShare(0)).toBe('0%')
    expect(formatProjectShare(0.0004)).toBe('<0.1%')
    expect(formatProjectShare(0.001)).toBe('0.1%')
    expect(formatProjectShare(0.857)).toBe('85.7%')
    expect(formatFastShare(0)).toBe('—')
    expect(formatFastShare(0.0004)).toBe('<0.1%')
  })

  it('names the top two models and counts the rest', () => {
    const summary = summarizeProjectKeys(statsOf([
      row(1, 'a', 'standard', '2026-10-01 01:00', 70),
      row(1, 'b', 'standard', '2026-10-01 01:00', 20),
      row(1, 'c', 'standard', '2026-10-01 01:00', 10),
    ]))[0]
    expect(formatTopModels(summary.models)).toBe('a 70.0% · b 20.0% 等 3 个')
    expect(formatTopModels(summary.models.slice(0, 2))).toBe('a 70.0% · b 20.0%')
  })
})

describe('buildProjectAxis', () => {
  it('spans the coverage even when records sit in a single bucket', () => {
    const axis = buildProjectAxis(statsOf(
      [row(1, 'm', 'standard', '2026-10-01 05:00', 1)],
      { start: '2026-10-01 03:00', end: '2026-10-01 07:00' }
    ))
    expect(axis).toEqual([
      '2026-10-01 03:00',
      '2026-10-01 04:00',
      '2026-10-01 05:00',
      '2026-10-01 06:00',
      '2026-10-01 07:00',
    ])
  })

  it('walks across midnight and month ends', () => {
    const axis = buildProjectAxis(statsOf(
      [row(1, 'm', 'standard', '2026-10-31 23:00', 1)],
      { start: '2026-10-31 22:00', end: '2026-11-01 01:00' }
    ))
    expect(axis).toEqual(['2026-10-31 22:00', '2026-10-31 23:00', '2026-11-01 00:00', '2026-11-01 01:00'])
  })

  it('extends past the coverage when a record falls outside it', () => {
    const axis = buildProjectAxis(statsOf(
      [row(1, 'm', 'standard', '2026-10-01 01:00', 1)],
      { start: '2026-10-01 03:00', end: '2026-10-01 04:00' }
    ))
    expect(axis).toEqual(['2026-10-01 01:00', '2026-10-01 02:00', '2026-10-01 03:00', '2026-10-01 04:00'])
  })

  it('ignores an inverted coverage and is empty without records', () => {
    expect(buildProjectAxis(statsOf(
      [row(1, 'm', 'standard', '2026-10-01 01:00', 1)],
      { start: '2026-10-05 00:00', end: '2026-10-01 00:00' }
    ))).toEqual(['2026-10-01 01:00'])
    expect(buildProjectAxis(statsOf([]))).toEqual([])
  })

  it('is not affected by the browser timezone around a DST change', () => {
    // 美国东部 2026-03-08 02:00 拨快：标签只是日历值，这一小时也要在轴上。
    const original = process.env.TZ
    process.env.TZ = 'America/New_York'
    try {
      const axis = buildProjectAxis(statsOf(
        [row(1, 'm', 'standard', '2026-03-08 01:00', 1)],
        { start: '2026-03-08 01:00', end: '2026-03-08 03:00' }
      ))
      expect(axis).toEqual(['2026-03-08 01:00', '2026-03-08 02:00', '2026-03-08 03:00'])
    } finally {
      process.env.TZ = original
    }
  })
})

describe('buildProjectTrend', () => {
  const axis = ['2026-10-01 22:00', '2026-10-01 23:00', '2026-10-02 00:00', '2026-10-02 01:00']
  const rows = [
    row(1, 'a', 'standard', '2026-10-01 22:00', 10),
    row(1, 'a', 'fast', '2026-10-01 22:00', 5),
    row(1, 'b', 'standard', '2026-10-02 01:00', 7),
  ]

  it('fills gaps with zero points per hour', () => {
    const trend = buildProjectTrend(rows, axis, 'hour')
    expect(trend.map((p) => [p.date, p.total_tokens])).toEqual([
      ['2026-10-01 22:00', 15],
      ['2026-10-01 23:00', 0],
      ['2026-10-02 00:00', 0],
      ['2026-10-02 01:00', 7],
    ])
    expect(trend[1]).toMatchObject({ requests: 0, input_tokens: 0, cost: 0, actual_cost: 0 })
  })

  it('rolls up to days by the bucket date', () => {
    const trend = buildProjectTrend(rows, axis, 'day')
    expect(trend.map((p) => [p.date, p.total_tokens, p.requests])).toEqual([
      ['2026-10-01', 15, 2],
      ['2026-10-02', 7, 1],
    ])
  })

  it('draws a key with no records in the range as all zero', () => {
    expect(buildProjectTrend([], axis, 'day').map((p) => p.total_tokens)).toEqual([0, 0])
  })
})

describe('coverage helpers', () => {
  it('detects a range entirely outside the coverage', () => {
    expect(isOutsideCoverage({ start: '2026-10-03 19:00', end: '2026-09-01 23:00' })).toBe(true)
    expect(isOutsideCoverage({ start: '2026-10-03 19:00', end: '2026-10-03 19:00' })).toBe(false)
  })

  it('reports the earliest time only when the selected start was clamped', () => {
    expect(clampedCoverageStart({ start: '2026-10-03 19:00', end: '2026-11-03 10:00' }, '2026-09-01')).toBe('2026-10-03 19:00')
    expect(clampedCoverageStart({ start: '2026-10-05 00:00', end: '2026-11-03 10:00' }, '2026-10-05')).toBeNull()
    expect(clampedCoverageStart({ start: '2026-10-03 19:00', end: '2026-09-01 23:00' }, '2026-09-01')).toBeNull()
  })
})

describe('defaultProjectGranularity', () => {
  it('uses hours for a day or less, days otherwise', () => {
    expect(defaultProjectGranularity('2026-10-01', '2026-10-01')).toBe('hour')
    expect(defaultProjectGranularity('2026-10-01', '2026-10-02')).toBe('hour')
    expect(defaultProjectGranularity('2026-10-01', '2026-10-03')).toBe('day')
  })
})
