<template>
  <div>
    <label class="input-label">{{ t('admin.accounts.kongSessionLimit.label') }}</label>
    <input
      :value="modelValue ?? ''"
      type="number"
      min="1"
      step="1"
      class="input"
      data-testid="openai-max-sessions"
      :placeholder="t('admin.accounts.kongSessionLimit.placeholder')"
      @input="onInput"
    />
    <p class="input-hint">{{ t('admin.accounts.kongSessionLimit.hint') }}</p>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import { normalizeMaxSessions } from './sessionLimit'

defineProps<{ modelValue: number | null }>()
const emit = defineEmits<{ 'update:modelValue': [value: number | null] }>()
const { t } = useI18n()

function onInput(event: Event) {
  emit('update:modelValue', normalizeMaxSessions((event.target as HTMLInputElement).value))
}
</script>
