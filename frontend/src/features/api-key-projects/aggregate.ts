import type { TrendDataPoint } from '@/types'
import type {
  KongApiKeyProjectCoverage,
  KongApiKeyProjectRow,
  KongApiKeyProjectStats,
  KongApiKeyProjectTier,
} from './api'

export type ProjectGranularity = 'day' | 'hour'

export const PROJECT_TIER_LABELS: Record<KongApiKeyProjectTier, string> = {
  standard: '标准',
  fast: 'Fast',
  ultrafast: 'Ultrafast',
  flex: 'Flex',
}

const TIER_ORDER: KongApiKeyProjectTier[] = ['standard', 'fast', 'ultrafast', 'flex']

export interface ProjectTotals {
  requests: number
  input_tokens: number
  output_tokens: number
  cache_creation_tokens: number
  cache_read_tokens: number
  total_tokens: number
  cost: number
  actual_cost: number
}

export interface ProjectTierStat extends ProjectTotals {
  tier: KongApiKeyProjectTier
  /** token 占该 key 合计的比例，合计为 0 时是 null */
  share: number | null
}

export interface ProjectModelStat extends ProjectTotals {
  model: string
  /** token 占该 key 合计的比例，合计为 0 时是 null */
  share: number | null
  tiers: ProjectTierStat[]
  /** 存在非标准档位时才列档位子行 */
  showTiers: boolean
}

export interface ProjectKeySummary extends ProjectTotals {
  api_key_id: number
  name: string
  deleted: boolean
  models: ProjectModelStat[]
  /** Fast 与 Ultrafast 的 token 占该 key 合计的比例，合计为 0 时是 null */
  fastShare: number | null
}

const emptyTotals = (): ProjectTotals => ({
  requests: 0,
  input_tokens: 0,
  output_tokens: 0,
  cache_creation_tokens: 0,
  cache_read_tokens: 0,
  total_tokens: 0,
  cost: 0,
  actual_cost: 0,
})

function addRow(totals: ProjectTotals, row: KongApiKeyProjectRow): void {
  totals.requests += row.requests
  totals.input_tokens += row.input_tokens
  totals.output_tokens += row.output_tokens
  totals.cache_creation_tokens += row.cache_creation_tokens
  totals.cache_read_tokens += row.cache_read_tokens
  totals.total_tokens += row.input_tokens + row.output_tokens + row.cache_creation_tokens + row.cache_read_tokens
  totals.cost += row.cost
  totals.actual_cost += row.actual_cost
}

const shareOf = (part: number, total: number): number | null => (total > 0 ? part / total : null)

function summarizeKey(key: KongApiKeyProjectStats['api_keys'][number], rows: KongApiKeyProjectRow[]): ProjectKeySummary {
  const totals = emptyTotals()
  const models = new Map<string, Map<KongApiKeyProjectTier, ProjectTotals>>()
  for (const row of rows) {
    addRow(totals, row)
    let tiers = models.get(row.model)
    if (!tiers) {
      tiers = new Map()
      models.set(row.model, tiers)
    }
    let tierTotals = tiers.get(row.tier)
    if (!tierTotals) {
      tierTotals = emptyTotals()
      tiers.set(row.tier, tierTotals)
    }
    addRow(tierTotals, row)
  }

  let fastTokens = 0
  const modelStats = [...models.entries()].map(([model, tiers]): ProjectModelStat => {
    const modelTotals = emptyTotals()
    const tierStats = TIER_ORDER.flatMap((tier): ProjectTierStat[] => {
      const tierTotals = tiers.get(tier)
      if (!tierTotals) return []
      for (const field of Object.keys(modelTotals) as (keyof ProjectTotals)[]) {
        modelTotals[field] += tierTotals[field]
      }
      if (tier === 'fast' || tier === 'ultrafast') fastTokens += tierTotals.total_tokens
      return [{ ...tierTotals, tier, share: shareOf(tierTotals.total_tokens, totals.total_tokens) }]
    })
    return {
      ...modelTotals,
      model,
      share: shareOf(modelTotals.total_tokens, totals.total_tokens),
      tiers: tierStats,
      showTiers: tierStats.some((stat) => stat.tier !== 'standard'),
    }
  })
  modelStats.sort((a, b) => b.total_tokens - a.total_tokens || a.model.localeCompare(b.model))

  return {
    ...totals,
    api_key_id: key.api_key_id,
    name: key.api_key_name || `#${key.api_key_id}`,
    deleted: key.deleted,
    models: modelStats,
    fastShare: shareOf(fastTokens, totals.total_tokens),
  }
}

/** 按 key 汇总，token 合计降序，相同时按 id 升序。 */
export function summarizeProjectKeys(stats: KongApiKeyProjectStats): ProjectKeySummary[] {
  return stats.api_keys
    .map((key) => summarizeKey(key, stats.rows.filter((row) => row.api_key_id === key.api_key_id)))
    .sort((a, b) => b.total_tokens - a.total_tokens || a.api_key_id - b.api_key_id)
}

const HOUR_MS = 60 * 60 * 1000

// 桶标签是已经换算好时区的字符串，这里只把它当日历值逐小时递增，借 UTC 计算以避开浏览器时区。
const parseBucket = (bucket: string): number =>
  Date.UTC(+bucket.slice(0, 4), +bucket.slice(5, 7) - 1, +bucket.slice(8, 10), +bucket.slice(11, 13))

const formatBucket = (ms: number): string => {
  const iso = new Date(ms).toISOString()
  return `${iso.slice(0, 10)} ${iso.slice(11, 13)}:00`
}

/**
 * 所有 key 共用的小时时间轴：可查范围与实际桶的并集。没有数据时是空的。
 * 图表的横轴是类别轴，缺的桶不补就看不出中间停用过；可查范围之外不补，超出保留期的日期不会被画成 0。
 */
export function buildProjectAxis(stats: KongApiKeyProjectStats): string[] {
  if (stats.rows.length === 0) return []
  let start = stats.rows[0].bucket
  let end = start
  for (const { bucket } of stats.rows) {
    if (bucket < start) start = bucket
    if (bucket > end) end = bucket
  }
  const { start: coverageStart, end: coverageEnd } = stats.coverage
  if (coverageStart <= coverageEnd) {
    if (coverageStart < start) start = coverageStart
    if (coverageEnd > end) end = coverageEnd
  }
  const axis: string[] = []
  const last = parseBucket(end)
  for (let ms = parseBucket(start); ms <= last; ms += HOUR_MS) {
    axis.push(formatBucket(ms))
  }
  return axis
}

/** 把一个 key 的行汇总到时间轴上，没有数据的桶是 0。按天时取桶标签的日期部分。 */
export function buildProjectTrend(
  rows: KongApiKeyProjectRow[],
  axis: string[],
  granularity: ProjectGranularity
): TrendDataPoint[] {
  const labelOf = (bucket: string) => (granularity === 'day' ? bucket.slice(0, 10) : bucket)
  const points = new Map<string, TrendDataPoint>()
  for (const bucket of axis) {
    const date = labelOf(bucket)
    if (!points.has(date)) points.set(date, { date, ...emptyTotals() })
  }
  for (const row of rows) {
    const point = points.get(labelOf(row.bucket))
    if (point) addRow(point, row)
  }
  return [...points.values()]
}

/** 默认粒度与用量页相同：范围不超过 1 天按小时，否则按天。 */
export function defaultProjectGranularity(startDate: string, endDate: string): ProjectGranularity {
  const days = (new Date(`${endDate}T00:00:00`).getTime() - new Date(`${startDate}T00:00:00`).getTime()) / (24 * HOUR_MS)
  return Math.ceil(days) <= 1 ? 'hour' : 'day'
}

/** 所选日期完全不在可查范围内（已清理，或是未来的日期）。 */
export const isOutsideCoverage = (coverage: KongApiKeyProjectCoverage): boolean => coverage.start > coverage.end

/** 所选起点早于最早可查的时刻时，返回最早可查的时刻。 */
export function clampedCoverageStart(coverage: KongApiKeyProjectCoverage, startDate: string): string | null {
  if (isOutsideCoverage(coverage)) return null
  return coverage.start > `${startDate} 00:00` ? coverage.start : null
}

export function formatProjectShare(share: number | null): string {
  if (share === null) return '—'
  if (share === 0) return '0%'
  if (share < 0.001) return '<0.1%'
  return `${(share * 100).toFixed(1)}%`
}

export const formatFastShare = (share: number | null): string => (share ? formatProjectShare(share) : '—')

export function formatProjectCost(value: number): string {
  if (value >= 1000) return `$${(value / 1000).toFixed(2)}K`
  if (value >= 1) return `$${value.toFixed(2)}`
  if (value >= 0.01) return `$${value.toFixed(3)}`
  return `$${value.toFixed(4)}`
}

/** 总览"模型"列的文字：token 占比前两名，更多的写"等 N 个"。 */
export function formatTopModels(models: ProjectModelStat[]): string {
  const top = models
    .slice(0, 2)
    .map((stat) => (stat.share === null ? stat.model : `${stat.model} ${formatProjectShare(stat.share)}`))
    .join(' · ')
  return models.length > 2 ? `${top} 等 ${models.length} 个` : top
}
