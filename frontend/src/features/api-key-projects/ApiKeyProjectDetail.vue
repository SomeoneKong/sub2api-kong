<template>
  <div class="space-y-4">
    <div class="overflow-x-auto rounded-lg border border-gray-100 dark:border-dark-700">
      <table class="w-full min-w-[720px] text-xs">
        <thead>
          <tr class="text-gray-500 dark:text-gray-400">
            <th class="px-3 py-2 text-left font-medium">模型 / 档位</th>
            <th class="px-3 py-2 text-right font-medium">请求</th>
            <th class="px-3 py-2 text-right font-medium">输入</th>
            <th class="px-3 py-2 text-right font-medium">输出</th>
            <th class="px-3 py-2 text-right font-medium">缓存</th>
            <th class="px-3 py-2 text-right font-medium">合计</th>
            <th class="w-44 px-3 py-2 text-left font-medium">占比</th>
            <th class="px-3 py-2 text-right font-medium">实际</th>
            <th class="px-3 py-2 text-right font-medium">标准</th>
          </tr>
        </thead>
        <tbody>
          <template v-for="stat in summary.models" :key="stat.model">
            <tr class="border-t border-gray-100 text-gray-900 dark:border-dark-700 dark:text-white" data-testid="model-row">
              <td class="px-3 py-1.5 font-medium">{{ stat.model }}</td>
              <td class="px-3 py-1.5 text-right">{{ stat.requests.toLocaleString() }}</td>
              <td class="px-3 py-1.5 text-right">{{ formatCompactNumber(stat.input_tokens) }}</td>
              <td class="px-3 py-1.5 text-right">{{ formatCompactNumber(stat.output_tokens) }}</td>
              <td class="px-3 py-1.5 text-right">{{ formatCompactNumber(cacheTokens(stat)) }}</td>
              <td class="px-3 py-1.5 text-right">{{ formatCompactNumber(stat.total_tokens) }}</td>
              <td class="px-3 py-1.5">
                <div class="flex items-center gap-2">
                  <div class="flex h-2 min-w-0 flex-1 overflow-hidden rounded bg-gray-100 dark:bg-dark-700">
                    <div
                      v-for="tier in stat.tiers"
                      :key="tier.tier"
                      :class="TIER_BAR_CLASSES[tier.tier]"
                      :style="tierBarStyle(tier)"
                      :title="`${PROJECT_TIER_LABELS[tier.tier]} ${formatProjectShare(tier.share)}`"
                    ></div>
                  </div>
                  <span class="w-12 shrink-0 text-right">{{ formatProjectShare(stat.share) }}</span>
                </div>
              </td>
              <td class="px-3 py-1.5 text-right text-green-600 dark:text-green-400">{{ formatProjectCost(stat.actual_cost) }}</td>
              <td class="px-3 py-1.5 text-right text-gray-400 dark:text-gray-500">{{ formatProjectCost(stat.cost) }}</td>
            </tr>
            <template v-if="stat.showTiers">
              <tr
                v-for="tier in stat.tiers"
                :key="`${stat.model}-${tier.tier}`"
                class="text-gray-600 dark:text-gray-400"
                data-testid="tier-row"
              >
                <td class="py-1 pl-6 pr-3">
                  <span class="mr-1.5 inline-block h-2 w-2 rounded-full" :class="TIER_BAR_CLASSES[tier.tier]"></span>{{ PROJECT_TIER_LABELS[tier.tier] }}
                </td>
                <td class="px-3 py-1 text-right">{{ tier.requests.toLocaleString() }}</td>
                <td class="px-3 py-1 text-right">{{ formatCompactNumber(tier.input_tokens) }}</td>
                <td class="px-3 py-1 text-right">{{ formatCompactNumber(tier.output_tokens) }}</td>
                <td class="px-3 py-1 text-right">{{ formatCompactNumber(cacheTokens(tier)) }}</td>
                <td class="px-3 py-1 text-right">{{ formatCompactNumber(tier.total_tokens) }}</td>
                <td class="px-3 py-1 text-right">{{ formatProjectShare(tier.share) }}</td>
                <td class="px-3 py-1 text-right text-green-600 dark:text-green-400">{{ formatProjectCost(tier.actual_cost) }}</td>
                <td class="px-3 py-1 text-right text-gray-400 dark:text-gray-500">{{ formatProjectCost(tier.cost) }}</td>
              </tr>
            </template>
          </template>
        </tbody>
      </table>
    </div>

    <p v-if="earliest" class="text-xs text-amber-600 dark:text-amber-400" data-testid="coverage-hint">
      最早可查到 {{ earliest }}，更早的明细已清理。
    </p>
    <TokenUsageTrend :trend-data="trend" />
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import TokenUsageTrend from '@/components/charts/TokenUsageTrend.vue'
import { formatCompactNumber } from '@/utils/format'
import type { KongApiKeyProjectRow, KongApiKeyProjectTier } from './api'
import {
  PROJECT_TIER_LABELS,
  buildProjectTrend,
  formatProjectCost,
  formatProjectShare,
  type ProjectGranularity,
  type ProjectKeySummary,
  type ProjectTierStat,
  type ProjectTotals,
} from './aggregate'

const props = defineProps<{
  summary: ProjectKeySummary
  rows: KongApiKeyProjectRow[]
  axis: string[]
  granularity: ProjectGranularity
  /** 所选起点早于最早可查的时刻时，是最早可查的时刻 */
  earliest: string | null
}>()

const TIER_BAR_CLASSES: Record<KongApiKeyProjectTier, string> = {
  standard: 'bg-blue-500',
  fast: 'bg-amber-500',
  ultrafast: 'bg-rose-500',
  flex: 'bg-emerald-500',
}

const cacheTokens = (stat: ProjectTotals): number => stat.cache_read_tokens + stat.cache_creation_tokens

// 比例很小的档位（线上 Fast 常在 1% 以下）也留出能看见的宽度。
const tierBarStyle = (tier: ProjectTierStat) => ({
  width: `${(tier.share ?? 0) * 100}%`,
  minWidth: tier.total_tokens > 0 ? '2px' : undefined,
})

const trend = computed(() => buildProjectTrend(props.rows, props.axis, props.granularity))
</script>
