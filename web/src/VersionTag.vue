<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'

// Stamped at build time from the repo's VERSION file ("dev" when running locally).
const front: string = import.meta.env.VITE_APP_VERSION || 'dev'

interface Info { version: string; commit: string; built_at: string }
const server = ref<Info | null>(null)
const failed = ref(false)

onMounted(async () => {
  try {
    const res = await fetch('/api/version', { cache: 'no-store' })
    if (res.ok) server.value = await res.json()
    else failed.value = true
  } catch {
    failed.value = true
  }
})

const built = computed(() => {
  const s = server.value?.built_at
  if (!s) return ''
  return new Intl.DateTimeFormat('zh-TW', {
    timeZone: 'Asia/Taipei', month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit', hour12: false,
  }).format(new Date(s))
})

const reload = () => window.location.reload()

// A home-screen app can keep showing an old page after the server was updated.
const stale = computed(() => !!server.value && front !== 'dev' && server.value.version !== front)
</script>

<template>
  <footer class="ver" aria-label="版本資訊">
    <p v-if="stale" class="stale" role="alert">
      畫面是舊版(v{{ front }}),伺服器已更新到 v{{ server?.version }}。
      <button type="button" @click="reload">重新載入</button>
    </p>
    <p v-if="server">
      版本 v{{ server.version }}<template v-if="server.commit"> · {{ server.commit }}</template><template v-if="built"> · 建置 {{ built }}</template>
    </p>
    <p v-else-if="failed">版本 v{{ front }}(伺服器版本讀取失敗)</p>
    <p v-else>版本 v{{ front }}</p>
  </footer>
</template>

<style scoped>
.ver { text-align: center; font-size: 0.75rem; opacity: 0.6; padding: 8px 16px calc(var(--safe-bottom) + 12px); font-variant-numeric: tabular-nums; }
.ver p { margin: 2px 0; }
.stale { opacity: 1; border-left: 3px solid var(--signal); display: inline-block; text-align: left; padding: 4px 8px; margin: 0 0 6px; }
.stale button { margin-left: 6px; font: inherit; color: var(--ink); background: none; border: 0; text-decoration: underline; cursor: pointer; }
</style>
