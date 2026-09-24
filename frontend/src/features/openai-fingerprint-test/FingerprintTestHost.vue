<template>
  <FingerprintTestModal v-if="account" :key="account.id" :account="account" @close="closeFingerprintTest" />
</template>

<script setup lang="ts">
import { onUnmounted } from 'vue'
import FingerprintTestModal from './FingerprintTestModal.vue'
import { closeFingerprintTest, fingerprintTestAccount as account } from './state'

// 指纹测试弹窗的宿主，在账号页挂一次。离开页面时清掉状态，免得回来时弹窗自己又打开。
onUnmounted(closeFingerprintTest)
</script>
