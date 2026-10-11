<template>
  <span
    v-if="hold"
    class="inline-flex items-center rounded px-1.5 py-0.5 text-xs font-medium"
    :class="
      hold.stage === 'confirmed'
        ? 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400'
        : 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-400'
    "
    :title="guardTooltip(hold)"
  >
    {{ guardBadgeText(hold) }}
  </span>
</template>

<script setup lang="ts">
import { computed } from 'vue'

import type { Account } from '@/types'
import { downgradeGuardHold, guardBadgeText, guardTooltip } from './guardHold'

// 账号列表状态列里的降档哨兵标记：只在哨兵关了调度的账号上出现，悬停看停调时间、影响时段与打开方法。
const props = defineProps<{ account: Account }>()

const hold = computed(() => downgradeGuardHold(props.account))
</script>
