<template>
  <button
    v-if="isFingerprintTestable(account)"
    class="flex w-full items-center gap-2 px-4 py-2 text-sm hover:bg-gray-100 dark:hover:bg-dark-700"
    @click="start"
  >
    <Icon name="beaker" size="sm" class="text-cyan-500" :stroke-width="2" />
    指纹测试
  </button>
</template>

<script setup lang="ts">
import { Icon } from '@/components/icons'
import type { Account } from '@/types'
import { isFingerprintTestable, openFingerprintTest } from './state'

// 账号行菜单里的「指纹测试」一项。弹窗不挂在这里：菜单关闭时本组件随之卸载。
const props = defineProps<{ account: Account }>()
const emit = defineEmits<{ close: [] }>()

function start() {
  openFingerprintTest(props.account)
  emit('close')
}
</script>
