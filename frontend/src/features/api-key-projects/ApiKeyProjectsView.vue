<template>
  <AppLayout>
    <div class="space-y-4">
      <div class="card p-4">
        <div class="flex flex-wrap items-center gap-4">
          <div class="flex items-center gap-2">
            <span class="text-sm font-medium text-gray-700 dark:text-gray-300">时间范围:</span>
            <DateRangePicker :start-date="startDate" :end-date="endDate" @change="onRangeChange" />
          </div>
          <div class="ml-auto inline-flex rounded-lg border border-gray-200 bg-gray-50 p-0.5 dark:border-dark-700 dark:bg-dark-800">
            <button
              v-for="option in GRANULARITY_OPTIONS"
              :key="option.value"
              type="button"
              class="rounded-md px-2.5 py-1 text-xs font-medium transition-colors"
              :class="granularity === option.value
                ? 'bg-white text-gray-900 shadow-sm dark:bg-dark-700 dark:text-white'
                : 'text-gray-500 hover:text-gray-700 dark:text-gray-400 dark:hover:text-gray-200'"
              :data-testid="`granularity-${option.value}`"
              @click="granularity = option.value"
            >
              {{ option.label }}
            </button>
          </div>
        </div>
        <div class="mt-3 space-y-0.5 text-xs text-gray-500 dark:text-gray-400">
          <p>明细只保留 31 天。请求数按用量记录统计，含中断后仍记了用量的请求和搜索等 0 token 的调用。</p>
          <p>Fast 按用量记录统计，实际扣费看费用列，不能据此确认上游是否提速。标准含未指定档位的请求。</p>
        </div>
      </div>

      <div v-if="loading" class="card flex h-48 items-center justify-center" data-testid="loading">
        <LoadingSpinner />
      </div>
      <div v-else-if="failed" class="card p-8 text-center" role="alert" data-testid="failed">
        <p class="text-sm text-red-600 dark:text-red-400">加载失败</p>
        <button type="button" class="btn btn-secondary btn-sm mt-3" data-testid="retry" @click="fetchStats">重试</button>
      </div>
      <div
        v-else-if="summaries.length === 0"
        class="card p-8 text-center text-sm text-gray-500 dark:text-gray-400"
        data-testid="empty"
      >
        {{ emptyMessage }}
      </div>
      <div v-else class="card overflow-x-auto">
        <table class="w-full min-w-[820px] text-sm">
          <thead>
            <tr class="text-xs text-gray-500 dark:text-gray-400">
              <th class="px-4 py-3 text-left font-medium">名称</th>
              <th class="px-4 py-3 text-right font-medium">请求</th>
              <th class="px-4 py-3 text-right font-medium">Token</th>
              <th class="px-4 py-3 text-left font-medium">模型</th>
              <th class="px-4 py-3 text-right font-medium" title="Fast 与 Ultrafast 的 token 占比">Fast</th>
              <th class="px-4 py-3 text-right font-medium">实际</th>
              <th class="px-4 py-3 text-right font-medium">标准</th>
            </tr>
          </thead>
          <tbody>
            <template v-for="summary in summaries" :key="summary.api_key_id">
              <tr
                class="cursor-pointer border-t border-gray-100 hover:bg-gray-50 dark:border-dark-700 dark:hover:bg-dark-800"
                data-testid="key-row"
                @click="toggle(summary.api_key_id)"
              >
                <td class="px-4 py-2.5 font-medium text-gray-900 dark:text-white">
                  <button
                    type="button"
                    class="flex min-w-0 items-center gap-1.5 text-left"
                    :aria-expanded="expanded.has(summary.api_key_id)"
                  >
                    <Icon
                      name="chevronRight"
                      size="xs"
                      class="shrink-0 text-gray-400 transition-transform"
                      :class="{ 'rotate-90': expanded.has(summary.api_key_id) }"
                    />
                    <span class="truncate">{{ summary.name }}</span>
                    <span
                      v-if="summary.deleted"
                      class="inline-flex shrink-0 items-center rounded px-1 py-px text-[10px] font-medium leading-tight bg-rose-100 text-rose-600 ring-1 ring-inset ring-rose-200 dark:bg-rose-500/20 dark:text-rose-400 dark:ring-rose-500/30"
                    >{{ t('usage.errors.keyDeleted') }}</span>
                  </button>
                </td>
                <td class="px-4 py-2.5 text-right text-gray-600 dark:text-gray-400">{{ summary.requests.toLocaleString() }}</td>
                <td class="px-4 py-2.5 text-right text-gray-600 dark:text-gray-400">{{ formatCompactNumber(summary.total_tokens) }}</td>
                <td class="px-4 py-2.5 text-xs text-gray-600 dark:text-gray-400">{{ formatTopModels(summary.models) }}</td>
                <td class="px-4 py-2.5 text-right text-gray-600 dark:text-gray-400">{{ formatFastShare(summary.fastShare) }}</td>
                <td class="px-4 py-2.5 text-right text-green-600 dark:text-green-400">{{ formatProjectCost(summary.actual_cost) }}</td>
                <td class="px-4 py-2.5 text-right text-gray-400 dark:text-gray-500">{{ formatProjectCost(summary.cost) }}</td>
              </tr>
              <tr v-if="expanded.has(summary.api_key_id)" data-testid="detail-row">
                <td colspan="7" class="px-4 pb-4 pt-1">
                  <ApiKeyProjectDetail
                    :summary="summary"
                    :rows="rowsOf(summary.api_key_id)"
                    :axis="axis"
                    :granularity="granularity"
                    :earliest="earliest"
                  />
                </td>
              </tr>
            </template>
          </tbody>
        </table>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import DateRangePicker from '@/components/common/DateRangePicker.vue'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import Icon from '@/components/icons/Icon.vue'
import { formatCompactNumber } from '@/utils/format'
import ApiKeyProjectDetail from './ApiKeyProjectDetail.vue'
import { useKongApiKeyProjects } from './api'
import {
  buildProjectAxis,
  clampedCoverageStart,
  defaultProjectGranularity,
  formatFastShare,
  formatProjectCost,
  formatTopModels,
  isOutsideCoverage,
  summarizeProjectKeys,
  type ProjectGranularity,
} from './aggregate'

// 文案用中文字面量：这个页面只服务本部署，不加进上游的 locale 文件。

const GRANULARITY_OPTIONS: { value: ProjectGranularity; label: string }[] = [
  { value: 'hour', label: '按小时' },
  { value: 'day', label: '按天' },
]

const { t } = useI18n()
const { stats, loading, failed, load, cancel } = useKongApiKeyProjects()

const formatLocalDate = (date: Date): string =>
  `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}-${String(date.getDate()).padStart(2, '0')}`

// 默认近 7 天，与日期选择器的"近 7 天"预设相同。
const today = new Date()
const weekAgo = new Date(today)
weekAgo.setDate(weekAgo.getDate() - 6)

const startDate = ref(formatLocalDate(weekAgo))
const endDate = ref(formatLocalDate(today))
const granularity = ref<ProjectGranularity>(defaultProjectGranularity(startDate.value, endDate.value))
const expanded = ref(new Set<number>())
let firstKeyExpanded = false

const summaries = computed(() => (stats.value ? summarizeProjectKeys(stats.value) : []))
const axis = computed(() => (stats.value ? buildProjectAxis(stats.value) : []))
const earliest = computed(() => (stats.value ? clampedCoverageStart(stats.value.coverage, startDate.value) : null))
const emptyMessage = computed(() =>
  stats.value && isOutsideCoverage(stats.value.coverage)
    ? '所选日期没有可查的明细（明细只保留 31 天）'
    : '可查范围内没有用量'
)

const rowsOf = (apiKeyId: number) => stats.value?.rows.filter((row) => row.api_key_id === apiKeyId) ?? []

const toggle = (apiKeyId: number) => {
  const next = new Set(expanded.value)
  if (!next.delete(apiKeyId)) next.add(apiKeyId)
  expanded.value = next
}

const fetchStats = async () => {
  await load({ start_date: startDate.value, end_date: endDate.value })
  if (!firstKeyExpanded && summaries.value.length > 0) {
    firstKeyExpanded = true
    toggle(summaries.value[0].api_key_id)
  }
}

const onRangeChange = (range: { startDate: string; endDate: string }) => {
  startDate.value = range.startDate
  endDate.value = range.endDate
  granularity.value = defaultProjectGranularity(range.startDate, range.endDate)
  void fetchStats()
}

onMounted(() => {
  void fetchStats()
})

onBeforeUnmount(cancel)
</script>
