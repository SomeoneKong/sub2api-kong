<template>
  <div class="card p-4">
    <div class="mb-4 flex items-center justify-between gap-3">
      <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
        {{ t('usage.apiKeyDistribution') }}
      </h3>
      <div class="inline-flex rounded-lg border border-gray-200 bg-gray-50 p-0.5 dark:border-dark-700 dark:bg-dark-800">
        <button
          type="button"
          class="rounded-md px-2.5 py-1 text-xs font-medium transition-colors"
          :class="metric === 'tokens'
            ? 'bg-white text-gray-900 shadow-sm dark:bg-dark-700 dark:text-white'
            : 'text-gray-500 hover:text-gray-700 dark:text-gray-400 dark:hover:text-gray-200'"
          @click="metric = 'tokens'"
        >
          {{ t('admin.dashboard.metricTokens') }}
        </button>
        <button
          type="button"
          class="rounded-md px-2.5 py-1 text-xs font-medium transition-colors"
          :class="metric === 'actual_cost'
            ? 'bg-white text-gray-900 shadow-sm dark:bg-dark-700 dark:text-white'
            : 'text-gray-500 hover:text-gray-700 dark:text-gray-400 dark:hover:text-gray-200'"
          @click="metric = 'actual_cost'"
        >
          {{ t('admin.dashboard.metricActualCost') }}
        </button>
      </div>
    </div>
    <div v-if="loading" class="flex h-48 items-center justify-center">
      <LoadingSpinner />
    </div>
    <div v-else-if="displayStats.length > 0" class="flex flex-col items-center gap-4 sm:flex-row sm:gap-6">
      <div class="h-48 w-48 shrink-0">
        <Doughnut :data="chartData" :options="doughnutOptions" />
      </div>
      <div class="max-h-48 w-full min-w-0 flex-1 overflow-auto">
        <table class="w-full text-xs">
          <thead>
            <tr class="text-gray-500 dark:text-gray-400">
              <th class="pb-2 text-left">{{ t('usage.apiKeyFilter') }}</th>
              <th class="pb-2 text-right">{{ t('admin.dashboard.requests') }}</th>
              <th class="pb-2 text-right">{{ t('admin.dashboard.tokens') }}</th>
              <th class="pb-2 text-right">{{ t('admin.dashboard.actual') }}</th>
              <th class="pb-2 text-right">{{ t('admin.dashboard.standard') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="(item, index) in displayStats"
              :key="item.api_key_id"
              class="border-t border-gray-100 dark:border-dark-700"
            >
              <td class="max-w-[140px] py-1.5 font-medium text-gray-900 dark:text-white" :title="keyLabel(item)">
                <span class="flex min-w-0 items-center gap-1.5">
                  <span class="h-2 w-2 shrink-0 rounded-full" :style="{ backgroundColor: colorAt(index) }"></span>
                  <span class="truncate">{{ keyName(item) }}</span>
                  <span
                    v-if="item.deleted"
                    class="inline-flex shrink-0 items-center rounded px-1 py-px text-[10px] font-medium leading-tight bg-rose-100 text-rose-600 ring-1 ring-inset ring-rose-200 dark:bg-rose-500/20 dark:text-rose-400 dark:ring-rose-500/30"
                  >{{ t('usage.errors.keyDeleted') }}</span>
                </span>
              </td>
              <td class="py-1.5 text-right text-gray-600 dark:text-gray-400">
                {{ formatNumber(item.requests) }}
              </td>
              <td class="py-1.5 text-right text-gray-600 dark:text-gray-400">
                {{ formatTokens(item.total_tokens) }}
              </td>
              <td class="py-1.5 text-right text-green-600 dark:text-green-400">
                ${{ formatCost(item.actual_cost) }}
              </td>
              <td class="py-1.5 text-right text-gray-400 dark:text-gray-500">
                ${{ formatCost(item.cost) }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
    <div
      v-else
      class="flex h-48 items-center justify-center text-sm text-gray-500 dark:text-gray-400"
    >
      {{ t('admin.dashboard.noDataAvailable') }}
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { Chart as ChartJS, ArcElement, Tooltip, Legend } from 'chart.js'
import { Doughnut } from 'vue-chartjs'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import type { KongApiKeyStat } from './api'

ChartJS.register(ArcElement, Tooltip, Legend)

const { t } = useI18n()

const props = withDefaults(defineProps<{
  stats: KongApiKeyStat[]
  loading?: boolean
}>(), {
  loading: false,
})

const metric = ref<'tokens' | 'actual_cost'>('tokens')

// 与分组分布图同一套颜色；key 可能多于 10 个，超出后循环使用。
const chartColors = [
  '#3b82f6',
  '#10b981',
  '#f59e0b',
  '#ef4444',
  '#8b5cf6',
  '#ec4899',
  '#14b8a6',
  '#f97316',
  '#6366f1',
  '#84cc16'
]

const colorAt = (index: number): string => chartColors[index % chartColors.length]

const metricValue = (item: KongApiKeyStat): number =>
  toFiniteNumber(metric.value === 'actual_cost' ? item.actual_cost : item.total_tokens)

const displayStats = computed(() =>
  [...(props.stats || [])].sort((a, b) => metricValue(b) - metricValue(a) || a.api_key_id - b.api_key_id)
)

const keyName = (item: KongApiKeyStat): string => item.api_key_name || `#${item.api_key_id}`

const keyLabel = (item: KongApiKeyStat): string =>
  item.deleted ? `${keyName(item)} (${t('usage.errors.keyDeleted')})` : keyName(item)

const chartData = computed(() => ({
  labels: displayStats.value.map(keyLabel),
  datasets: [
    {
      data: displayStats.value.map(metricValue),
      backgroundColor: displayStats.value.map((_, index) => colorAt(index)),
      borderWidth: 0
    }
  ]
}))

const doughnutOptions = computed(() => ({
  responsive: true,
  maintainAspectRatio: false,
  plugins: {
    legend: {
      display: false
    },
    tooltip: {
      callbacks: {
        label: (context: any) => {
          const value = context.raw as number
          const total = context.dataset.data.reduce((a: number, b: number) => a + b, 0)
          const percentage = total > 0 ? ((value / total) * 100).toFixed(1) : '0.0'
          const formattedValue = metric.value === 'actual_cost'
            ? `$${formatCost(value)}`
            : formatTokens(value)
          return `${context.label}: ${formattedValue} (${percentage}%)`
        }
      }
    }
  }
}))

const formatTokens = (value: number): string => {
  if (value >= 1_000_000_000) {
    return `${(value / 1_000_000_000).toFixed(2)}B`
  } else if (value >= 1_000_000) {
    return `${(value / 1_000_000).toFixed(2)}M`
  } else if (value >= 1_000) {
    return `${(value / 1_000).toFixed(2)}K`
  }
  return value.toLocaleString()
}

const formatNumber = (value: number): string => {
  return toFiniteNumber(value).toLocaleString()
}

const toFiniteNumber = (value: unknown): number => {
  const numberValue = Number(value)
  return Number.isFinite(numberValue) ? numberValue : 0
}

const formatCost = (value: number | null | undefined): string => {
  const safeValue = toFiniteNumber(value)
  if (safeValue >= 1000) {
    return (safeValue / 1000).toFixed(2) + 'K'
  } else if (safeValue >= 1) {
    return safeValue.toFixed(2)
  } else if (safeValue >= 0.01) {
    return safeValue.toFixed(3)
  }
  return safeValue.toFixed(4)
}
</script>
