import { ref } from 'vue'
import { apiClient } from '@/api/client'
import type { UsageQueryParams } from '@/types'

// 用量页「API 密钥使用分布」：后端按 API key 汇总当前用户在所选范围内的用量，筛选参数与统计卡片相同。

export interface KongApiKeyStat {
  api_key_id: number
  api_key_name: string
  /** key 已删除（软删除，名字仍在） */
  deleted: boolean
  requests: number
  total_tokens: number
  cost: number
  actual_cost: number
}

export interface KongApiKeyStatsResponse {
  api_keys: KongApiKeyStat[]
}

export async function getKongApiKeyStats(params: UsageQueryParams): Promise<KongApiKeyStatsResponse> {
  const { data } = await apiClient.get<KongApiKeyStatsResponse>('/usage/dashboard/kong-api-key-stats', { params })
  return data
}

// 与页面上其他图表的加载方式一致：筛选条件快速切换时，只保留最后一次请求的结果。
export function useKongApiKeyStats() {
  const stats = ref<KongApiKeyStat[]>([])
  const loading = ref(false)
  let seq = 0

  const load = async (params: UsageQueryParams) => {
    const current = ++seq
    loading.value = true
    try {
      const response = await getKongApiKeyStats(params)
      if (current !== seq) return
      stats.value = response.api_keys || []
    } catch (error) {
      if (current !== seq) return
      console.error('Failed to load API key stats:', error)
      stats.value = []
    } finally {
      if (current === seq) loading.value = false
    }
  }

  return { stats, loading, load }
}
