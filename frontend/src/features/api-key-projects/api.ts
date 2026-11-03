import { ref } from 'vue'
import { apiClient } from '@/api/client'

// API Key 项目统计：后端按 key、模型、档位、小时返回当前用户的用量，前端切分展示。设计见 DESIGN-api-key-project-stats.md。

/** 所选范围内明细可查的时段，起止都是小时桶标签。start 晚于 end 表示所选日期完全不可查。 */
export interface KongApiKeyProjectCoverage {
  start: string
  end: string
}

export interface KongApiKeyProjectKey {
  api_key_id: number
  api_key_name: string
  /** key 已删除（软删除，名字仍在） */
  deleted: boolean
}

export type KongApiKeyProjectTier = 'standard' | 'fast' | 'ultrafast' | 'flex'

export interface KongApiKeyProjectRow {
  api_key_id: number
  model: string
  /** 用量记录里的档位归类，不代表上游实际走的档位，也不代表实际收费 */
  tier: KongApiKeyProjectTier
  /** 小时桶标签，如 2026-11-03 14:00，已按筛选用的时区划分 */
  bucket: string
  /** 用量记录数 */
  requests: number
  input_tokens: number
  output_tokens: number
  cache_creation_tokens: number
  cache_read_tokens: number
  cost: number
  actual_cost: number
}

export interface KongApiKeyProjectStats {
  coverage: KongApiKeyProjectCoverage
  api_keys: KongApiKeyProjectKey[]
  rows: KongApiKeyProjectRow[]
}

export interface KongApiKeyProjectParams {
  start_date: string
  end_date: string
}

export async function getKongApiKeyProjects(
  params: KongApiKeyProjectParams,
  signal?: AbortSignal
): Promise<KongApiKeyProjectStats> {
  const { data } = await apiClient.get<KongApiKeyProjectStats>('/usage/dashboard/kong-api-key-projects', { params, signal })
  return data
}

// 查询要扫大半张明细表：发新请求和离开页面时取消旧请求，取消会传到数据库。
export function useKongApiKeyProjects() {
  const stats = ref<KongApiKeyProjectStats | null>(null)
  const loading = ref(false)
  const failed = ref(false)
  let controller: AbortController | null = null

  const cancel = () => {
    controller?.abort()
    controller = null
    loading.value = false
  }

  const load = async (params: KongApiKeyProjectParams) => {
    cancel()
    const current = new AbortController()
    controller = current
    loading.value = true
    failed.value = false
    stats.value = null
    try {
      const data = await getKongApiKeyProjects(params, current.signal)
      if (controller === current) stats.value = data
    } catch (error) {
      if (controller === current) {
        console.error('Failed to load API key project stats:', error)
        failed.value = true
      }
    } finally {
      if (controller === current) loading.value = false
    }
  }

  return { stats, loading, failed, load, cancel }
}
