<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { api, DELIVERY_REASON, DELIVERY_STATUS, leadLabel, type LogEntry, type LogFilter, type Usage } from './api'

const props = defineProps<{ usage: Usage | null }>()
const emit = defineEmits<{ (e: 'back'): void }>()

const FILTERS: { key: LogFilter; label: string }[] = [
  { key: 'all', label: '全部' },
  { key: 'sent', label: '已發送' },
  { key: 'failed', label: '失敗' },
  { key: 'pending', label: '待發送' },
  { key: 'skipped', label: '略過' },
]

const filter = ref<LogFilter>('all')
const entries = ref<LogEntry[]>([])
const cursor = ref('')
const loading = ref(false)
const loaded = ref(false)
const error = ref('')

// A stale answer from a previous filter must never overwrite a newer one.
let generation = 0

async function load(reset: boolean) {
  if (loading.value && !reset) return
  const mine = ++generation
  if (reset) {
    entries.value = []
    cursor.value = ''
    loaded.value = false
  }
  loading.value = true
  error.value = ''
  try {
    const page = await api.deliveries(filter.value, cursor.value)
    if (mine !== generation) return
    entries.value = [...entries.value, ...page.entries]
    cursor.value = page.next_cursor
  } catch {
    if (mine === generation) error.value = '讀取發送紀錄失敗,請再試一次。'
  } finally {
    if (mine === generation) {
      loading.value = false
      loaded.value = true
    }
  }
}

watch(filter, () => load(true))
onMounted(() => load(true))

const fmt = new Intl.DateTimeFormat('zh-TW', {
  timeZone: 'Asia/Taipei', month: 'numeric', day: 'numeric', weekday: 'short',
  hour: '2-digit', minute: '2-digit', hour12: false,
})
const when = (iso: string) => fmt.format(new Date(iso))

function detail(e: LogEntry): string {
  switch (e.status) {
    case 'sent':
      return `發送於 ${when(e.sent_at ?? e.due_at)}`
    case 'pending':
      return `預定 ${when(e.due_at)} 發送`
    case 'failed':
      return `預定 ${when(e.due_at)},已嘗試 ${e.attempts} 次`
    default:
      return `原定 ${when(e.due_at)}`
  }
}
</script>

<template>
  <div class="lv">
    <van-nav-bar title="發送紀錄">
      <template #left><van-button size="small" plain @click="emit('back')">返回</van-button></template>
    </van-nav-bar>

    <div class="wrap">
      <p v-if="props.usage" class="small">
        本月已發送 {{ props.usage.sent }}<template v-if="props.usage.limit"> / {{ props.usage.limit }}</template> 則(每位收件人算一則)。
      </p>

      <div class="chips" role="tablist" aria-label="篩選狀態">
        <button
          v-for="f in FILTERS"
          :key="f.key"
          type="button"
          role="tab"
          class="chip"
          :class="{ on: filter === f.key }"
          :aria-selected="filter === f.key"
          @click="filter = f.key"
        >{{ f.label }}</button>
      </div>

      <p v-if="error" class="err" role="alert">{{ error }}</p>

      <ul v-if="entries.length" class="rows">
        <li v-for="e in entries" :key="e.id">
          <div class="top">
            <strong>{{ e.contact_name }}</strong>
            <span class="title">{{ e.event_title }}</span>
            <em class="st" :class="e.status">{{ DELIVERY_STATUS[e.status] }}</em>
          </div>
          <div class="sub">
            {{ detail(e) }}
            <template v-if="e.late">(補發)</template>
          </div>
          <div class="sub">
            行程 {{ when(e.event_start) }} · 提前 {{ leadLabel(e.lead_minutes) }}
            <template v-if="e.reason && DELIVERY_REASON[e.reason]"> · {{ DELIVERY_REASON[e.reason] }}</template>
            <template v-else-if="e.reason"> · {{ e.reason }}</template>
          </div>
        </li>
      </ul>
      <p v-else-if="loaded && !loading && !error" class="empty">沒有符合的紀錄。</p>
      <p v-if="loading" class="small" aria-live="polite">讀取中…</p>

      <button v-if="cursor && !loading" type="button" class="more" @click="load(false)">載入更多</button>
      <p class="small foot">只記錄結果,不保存訊息內容。在 App 內刪除行程時,該行程的紀錄也會一併刪除。</p>
    </div>
  </div>
</template>

<style scoped>
.lv { padding-top: var(--safe-top); padding-bottom: calc(var(--safe-bottom) + 24px); }
.wrap { max-width: 32rem; margin: 0 auto; padding: 8px 16px 0; }
.small { font-size: 0.85rem; opacity: 0.7; margin: 6px 0; }
.foot { margin-top: 20px; }
.err { border-left: 3px solid var(--signal); padding-left: 10px; margin: 8px 0; line-height: 1.6; font-size: 0.92rem; }
.empty { text-align: center; opacity: 0.6; padding: 32px 0; }
.chips { display: flex; gap: 8px; flex-wrap: wrap; margin: 8px 0 12px; }
.chip { border: 1px solid color-mix(in srgb, var(--ink) 35%, transparent); background: var(--paper); color: var(--ink); border-radius: 999px; padding: 6px 14px; font: inherit; font-size: 0.9rem; cursor: pointer; }
.chip.on { background: var(--ink); color: var(--paper); }
.rows { list-style: none; margin: 0; padding: 0; }
.rows li { padding: 12px 0; border-bottom: 1px solid color-mix(in srgb, var(--ink) 12%, transparent); }
.top { display: flex; align-items: baseline; gap: 8px; flex-wrap: wrap; }
.title { opacity: 0.8; }
.st { font-style: normal; font-size: 0.75rem; margin-left: auto; padding: 1px 8px; border-radius: 999px; background: color-mix(in srgb, var(--ink) 10%, transparent); }
.st.sent { background: color-mix(in srgb, #2a9d62 25%, transparent); }
.st.failed { background: color-mix(in srgb, var(--signal) 25%, transparent); }
.sub { font-size: 0.85rem; opacity: 0.7; margin-top: 2px; font-variant-numeric: tabular-nums; }
.more { display: block; margin: 16px auto 0; border: 1px solid color-mix(in srgb, var(--ink) 35%, transparent); background: var(--paper); color: var(--ink); border-radius: 999px; padding: 8px 20px; font: inherit; cursor: pointer; }
</style>
