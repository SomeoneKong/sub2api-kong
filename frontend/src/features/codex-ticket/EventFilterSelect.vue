<!--
  事件筛选器的多选下拉。本页自有，不复用 channel-monitor-v2 的 FilterMultiSelect——那个绑在
  该 feature 的 i18n 命名空间与格式化模块上，本页全程硬编码中文、无 i18n。

  空选中即「全部」，与后端一致：那边把空的条件切片当作该维度不过滤。
-->
<template>
  <div ref="rootRef" class="relative">
    <button
      type="button"
      class="input flex items-center justify-between gap-2 text-left"
      :aria-expanded="open"
      aria-haspopup="listbox"
      @click="open = !open"
    >
      <span class="truncate">{{ summary }}</span>
      <span class="shrink-0 text-xs text-gray-400" :class="open ? 'rotate-180' : ''">▾</span>
    </button>

    <div
      v-if="open"
      class="absolute left-0 z-20 mt-1 max-h-72 w-full min-w-max overflow-auto rounded-md border border-gray-200 bg-white py-1 shadow-lg dark:border-dark-600 dark:bg-dark-800"
      role="listbox"
      aria-multiselectable="true"
    >
      <button
        type="button"
        class="flex w-full items-center gap-2 border-b border-gray-100 px-3 py-1.5 text-left text-sm hover:bg-gray-100 dark:border-dark-700 dark:hover:bg-dark-700"
        @click="selectAll"
      >
        <span class="w-4 text-primary-500">{{ modelValue.length === 0 ? '✓' : '' }}</span>
        <span>{{ allLabel }}</span>
      </button>
      <button
        v-for="opt in options"
        :key="opt.value"
        type="button"
        role="option"
        :aria-selected="modelValue.includes(opt.value)"
        class="flex w-full items-center gap-2 px-3 py-1.5 text-left text-sm hover:bg-gray-100 dark:hover:bg-dark-700"
        @click="toggle(opt.value)"
      >
        <span class="w-4 text-primary-500">{{ modelValue.includes(opt.value) ? '✓' : '' }}</span>
        <span>{{ opt.label }}</span>
      </button>
      <p v-if="options.length === 0" class="px-3 py-2 text-center text-xs text-gray-400">没有可选项。</p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'

const props = defineProps<{
  modelValue: string[]
  /** 空选中时显示的文字，同时也是「全部」那一项的标签。 */
  allLabel: string
  options: { value: string; label: string }[]
}>()

const emit = defineEmits<{
  'update:modelValue': [value: string[]]
  /** 面板关闭且选中项确有变化时触发一次。逐项勾选时不发——那会让表格连着重查好几遍。 */
  change: []
}>()

const rootRef = ref<HTMLElement | null>(null)
const open = ref(false)
// 打开那一刻的选中项，关闭时用来判断是否真的改过。
let snapshot: string[] = []

const summary = computed(() => {
  if (props.modelValue.length === 0) return props.allLabel
  if (props.modelValue.length === 1) {
    const hit = props.options.find((o) => o.value === props.modelValue[0])
    return hit ? hit.label : props.modelValue[0]
  }
  return `已选 ${props.modelValue.length} 项`
})

function toggle(value: string): void {
  const next = props.modelValue.includes(value)
    ? props.modelValue.filter((v) => v !== value)
    : [...props.modelValue, value]
  emit('update:modelValue', next)
}

function selectAll(): void {
  if (props.modelValue.length) emit('update:modelValue', [])
}

function onDocumentClick(ev: MouseEvent): void {
  if (!open.value) return
  const root = rootRef.value
  if (root && ev.target instanceof Node && !root.contains(ev.target)) open.value = false
}

function onKeydown(ev: KeyboardEvent): void {
  if (ev.key === 'Escape') open.value = false
}

watch(open, (isOpen) => {
  if (isOpen) {
    snapshot = [...props.modelValue]
    document.addEventListener('click', onDocumentClick)
    document.addEventListener('keydown', onKeydown)
    return
  }
  document.removeEventListener('click', onDocumentClick)
  document.removeEventListener('keydown', onKeydown)
  const changed =
    snapshot.length !== props.modelValue.length || snapshot.some((v) => !props.modelValue.includes(v))
  if (changed) emit('change')
})

onBeforeUnmount(() => {
  document.removeEventListener('click', onDocumentClick)
  document.removeEventListener('keydown', onKeydown)
})
</script>
