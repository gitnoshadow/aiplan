<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { api, ApiError, CATEGORIES, ERROR_TEXT, LEAD_OPTIONS, type Contact, type EventRow, type Group, type ParseResult, type Usage } from './api'
import { WavRecorder } from './recorder'
import ContactsView from './ContactsView.vue'

const props = defineProps<{ email: string; notice: string }>()
const emit = defineEmits<{ (e: 'logout'): void }>()

const MAX_RECORD_SECONDS = 60

// ---- Google connection -----------------------------------------------------
const google = reactive({ connected: true, needsReauth: false, loaded: false })
async function loadGoogle() {
  try {
    const s = await api.googleStatus()
    google.connected = s.connected
    google.needsReauth = s.needs_reauth
  } catch {
    /* banner just stays hidden */
  } finally {
    google.loaded = true
  }
}
const showConnect = computed(() => google.loaded && (!google.connected || google.needsReauth))

// ---- LINE binding ------------------------------------------------------------
const view = ref<'plans' | 'contacts'>('plans')
const contacts = ref<Contact[]>([])
const groups = ref<Group[]>([])
const usage = ref<Usage | null>(null)
const contactsLoaded = ref(false)
const lineBound = computed(() => contacts.value.some((c) => c.is_self && c.status === 'active'))
const lineBlocked = computed(() => contacts.value.some((c) => c.is_self && c.status === 'blocked'))
const usageWarn = computed(() => usage.value && usage.value.level !== 'ok')
const usageText = computed(() => {
  const u = usage.value
  if (!u) return ''
  if (u.level === 'full') return `本月 LINE 額度已用完(${u.sent}/${u.limit}),提醒不會發送,下個月自動恢復。`
  if (u.level === 'critical') return `本月 LINE 額度快用完了(${u.sent}/${u.limit})。`
  return `本月 LINE 額度已用 ${u.sent}/${u.limit} 則。`
})
const remaining = computed(() => (usage.value && usage.value.limit > 0 ? Math.max(0, usage.value.limit - usage.value.sent) : null))
const overQuota = computed(() => remaining.value !== null && remindTo.value.length > remaining.value)

// A group chip ticks (or unticks) all of its members who can receive messages.
function groupMembers(g: Group): number[] {
  return g.member_ids.filter((id) => activeContacts.value.some((c) => c.id === id))
}
const groupOn = (g: Group) => {
  const m = groupMembers(g)
  return m.length > 0 && m.every((id) => remindTo.value.includes(id))
}
function toggleGroup(g: Group) {
  const m = groupMembers(g)
  remindTo.value = groupOn(g)
    ? remindTo.value.filter((id) => !m.includes(id))
    : [...new Set([...remindTo.value, ...m])]
}
// Only people who can actually receive a message are offered as recipients.
const activeContacts = computed(() => contacts.value.filter((c) => c.status === 'active' && c.enabled))
const flash = ref('')

async function loadContacts() {
  try {
    const [c, g, u] = await Promise.all([api.contacts(), api.groups(), api.usage()])
    contacts.value = c
    groups.value = g
    usage.value = u
  } catch {
    /* keep previous */
  } finally {
    contactsLoaded.value = true
  }
}

const bind = reactive({ code: '', active: false, busy: false, error: '' })
let pollTimer: number | undefined
let pollUntil = 0

function stopPolling() {
  window.clearInterval(pollTimer)
  pollTimer = undefined
}

async function startBind() {
  bind.busy = true
  bind.error = ''
  try {
    const r = await api.bind()
    bind.code = r.code
    bind.active = true
    pollUntil = Date.now() + r.expires_in_minutes * 60_000
    stopPolling()
    pollTimer = window.setInterval(async () => {
      await loadContacts()
      if (lineBound.value) {
        stopPolling()
        bind.active = false
        flash.value = 'LINE 綁定成功,之後可以在確認畫面選擇要提醒你。'
      } else if (Date.now() > pollUntil) {
        stopPolling()
        bind.active = false
        bind.error = '綁定碼已過期,請重新產生。'
      }
    }, 3000)
  } catch {
    bind.error = '產生綁定碼失敗,請再試一次。'
  } finally {
    bind.busy = false
  }
}
const prettyCode = computed(() => (bind.code ? `${bind.code.slice(0, 4)} ${bind.code.slice(4)}` : ''))

// ---- Input (text + voice) --------------------------------------------------
const text = ref('')
const error = ref('')
const parsing = ref(false)
const recording = ref(false)
const transcribing = ref(false)
const seconds = ref(0)
const canRecord = WavRecorder.supported()
let recorder: WavRecorder | null = null
let timer: number | undefined

function showError(e: unknown, fallback = '發生錯誤,請再試一次。') {
  if (e instanceof ApiError) error.value = ERROR_TEXT[e.code] ?? fallback
  else error.value = fallback
}

async function toggleRecord() {
  error.value = ''
  if (recording.value) return stopRecord()
  try {
    recorder = new WavRecorder()
    await recorder.start()
  } catch {
    recorder = null
    error.value = '無法使用麥克風。請在 iPhone「設定」中允許此網站使用麥克風,或直接打字。'
    return
  }
  recording.value = true
  seconds.value = 0
  timer = window.setInterval(() => {
    seconds.value++
    if (seconds.value >= MAX_RECORD_SECONDS) void stopRecord()
  }, 1000)
}

async function stopRecord() {
  if (!recorder) return
  window.clearInterval(timer)
  recording.value = false
  transcribing.value = true
  try {
    const wav = await recorder.stop()
    recorder = null
    const { text: heard } = await api.transcribe(wav)
    if (!heard) error.value = '沒有聽到內容,請再說一次。'
    else text.value = text.value ? `${text.value} ${heard}` : heard
  } catch (e) {
    showError(e, '語音辨識失敗,請再試一次,或直接打字。')
  } finally {
    transcribing.value = false
  }
}

onBeforeUnmount(() => {
  document.removeEventListener('visibilitychange', onVisible)
  window.clearInterval(tickTimer)
  stopPolling()
  window.clearInterval(timer)
  recorder?.cancel()
})

// ---- Parse -> confirm ------------------------------------------------------
interface Draft {
  title: string
  start: string // datetime-local value, Asia/Taipei
  duration: number
  durationIsDefault: boolean
  location: string
  notes: string
  category: string
  ambiguities: { text: string; ok: boolean }[]
}
const draft = ref<Draft | null>(null)
const requestId = ref('')
const saving = ref(false)
const remindLead = ref(60)
const remindTo = ref<number[]>([]) // empty by default: sensitive plans never leak by accident

const newId = () =>
  crypto.randomUUID?.() ?? `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 12)}`

async function parse() {
  const t = text.value.trim()
  if (!t || parsing.value) return
  error.value = ''
  parsing.value = true
  try {
    const r: ParseResult = await api.parse(t)
    draft.value = {
      title: r.title,
      // The server formats in Asia/Taipei, so the first 16 chars are local wall time.
      start: r.start_time ? r.start_time.slice(0, 16) : '',
      duration: r.duration_minutes,
      durationIsDefault: r.duration_is_default,
      location: r.location,
      notes: r.notes,
      category: r.category,
      ambiguities: r.ambiguities.map((a) => ({ text: a, ok: false })),
    }
    requestId.value = newId()
    remindLead.value = 60
    remindTo.value = []
  } catch (e) {
    showError(e)
  } finally {
    parsing.value = false
  }
}

const canConfirm = computed(() => {
  const d = draft.value
  return !!d && d.title.trim() !== '' && d.start !== '' && d.duration >= 1 && d.duration <= 1440 &&
    d.ambiguities.every((a) => a.ok)
})

async function confirm() {
  const d = draft.value
  if (!d || !canConfirm.value || saving.value) return
  error.value = ''
  saving.value = true
  try {
    const res = await api.confirm({
      request_id: requestId.value,
      title: d.title,
      start_time: `${d.start}:00+08:00`,
      duration_minutes: d.duration,
      location: d.location,
      notes: d.notes,
      category: d.category,
      reminder: remindTo.value.length ? { lead_minutes: remindLead.value, contact_ids: remindTo.value } : null,
    })
    flash.value = reminderNotice(res.reminder, res.reminder_late, res.reminder_recipients)
    draft.value = null
    text.value = ''
    await Promise.all([loadEvents(), loadGoogle(), loadContacts()])
  } catch (e) {
    if (e instanceof ApiError && (e.code === 'reauth_required' || e.code === 'calendar_not_connected')) {
      google.connected = e.code !== 'calendar_not_connected'
      google.needsReauth = e.code === 'reauth_required'
      google.loaded = true
      error.value = ''
    } else showError(e)
  } finally {
    saving.value = false
  }
}

function reminderNotice(status?: string, late?: boolean, n?: number): string {
  const who = n && n > 1 ? `(${n} 人)` : ''
  switch (status) {
    case 'scheduled':
      return late ? `已寫入行事曆。提醒時間已過,會立刻發送 LINE 通知${who}。` : `已寫入行事曆,並排定 LINE 提醒${who}。`
    case 'event_started':
      return '已寫入行事曆。行程已經開始,所以沒有設定提醒。'
    case 'failed':
      return '已寫入行事曆,但提醒設定失敗,這筆行程不會收到 LINE 提醒。'
    default:
      return '已寫入行事曆。'
  }
}

function cancelDraft() {
  draft.value = null
  error.value = ''
}

// ---- Google Calendar sync --------------------------------------------------
const sync = reactive({ at: null as string | null, error: '', interval: 5, busy: false, msg: '' })
const nowTick = ref(Date.now())
let tickTimer: number | undefined

async function loadSync() {
  try {
    const s = await api.syncStatus()
    sync.at = s.last_synced_at
    sync.error = s.last_error
    sync.interval = s.interval_minutes
  } catch {
    /* keep previous */
  }
}
async function syncNow() {
  sync.busy = true
  sync.msg = ''
  try {
    const r = await api.syncNow()
    sync.msg = r.updated || r.cancelled ? `已同步:${r.updated} 筆更新、${r.cancelled} 筆取消。` : '已同步,沒有變動。'
    await Promise.all([loadEvents(), loadSync()])
  } catch (e) {
    if (e instanceof ApiError && e.code === 'reauth_required') google.needsReauth = true
    else sync.msg = e instanceof ApiError && e.code === 'calendar_not_connected' ? '還沒連結 Google 行事曆。' : '同步失敗,請稍後再試。'
  } finally {
    sync.busy = false
  }
}
const syncAgo = computed(() => {
  if (!sync.at) return '尚未同步'
  const m = Math.floor((nowTick.value - new Date(sync.at).getTime()) / 60000)
  return m < 1 ? '剛剛' : m < 60 ? `${m} 分鐘前` : `${Math.floor(m / 60)} 小時前`
})
const syncProblem = computed(() => {
  if (!sync.error) return ''
  return sync.error === 'reauth_required' ? '需要重新授權 Google 才能同步。' : '上次同步失敗,系統會自動重試。'
})

// Coming back to the app (e.g. from the home screen) should show fresh data.
function onVisible() {
  if (document.visibilityState === 'visible') {
    void loadEvents()
    void loadSync()
  }
}

// ---- Events ----------------------------------------------------------------
const events = ref<EventRow[]>([])
async function loadEvents() {
  try {
    events.value = await api.events()
  } catch {
    /* keep the previous list */
  }
}
async function remove(ev: EventRow) {
  if (!window.confirm(`刪除「${ev.title}」?Google 行事曆上的這筆行程也會一併刪除。`)) return
  try {
    await api.deleteEvent(ev.id)
    events.value = events.value.filter((e) => e.id !== ev.id)
  } catch (e) {
    if (e instanceof ApiError && e.code === 'reauth_required') google.needsReauth = true
    else showError(e)
  }
}

const fmt = new Intl.DateTimeFormat('zh-TW', {
  timeZone: 'Asia/Taipei', month: 'numeric', day: 'numeric', weekday: 'short',
  hour: '2-digit', minute: '2-digit', hour12: false,
})
const when = (iso: string) => fmt.format(new Date(iso))

onMounted(() => {
  void loadGoogle()
  void loadEvents()
  void loadContacts()
  void loadSync()
  document.addEventListener('visibilitychange', onVisible)
  tickTimer = window.setInterval(() => (nowTick.value = Date.now()), 30_000)
})
</script>

<template>
  <ContactsView
    v-if="view === 'contacts'"
    :contacts="contacts"
    :groups="groups"
    :usage="usage"
    @back="view = 'plans'"
    @changed="loadContacts"
  />
  <div v-else class="home">
    <van-nav-bar title="計畫">
      <template #left>
        <van-button size="small" plain @click="view = 'contacts'">聯絡人</van-button>
      </template>
      <template #right>
        <van-button size="small" plain @click="emit('logout')">登出</van-button>
      </template>
    </van-nav-bar>

    <div class="wrap">
      <p v-if="props.notice" class="banner ok" role="status">{{ props.notice }}</p>

      <p v-if="flash" class="banner ok" role="status">{{ flash }}</p>

      <div v-if="usageWarn" class="banner" :class="usage?.level === 'warn' ? 'line' : 'warn'" role="status">
        <span>{{ usageText }}</span>
      </div>

      <div v-if="contactsLoaded && !lineBound" class="banner line">
        <template v-if="lineBlocked">
          <span>你封鎖了官方帳號,無法收到提醒。請在 LINE 解除封鎖後,重新綁定。</span>
        </template>
        <template v-else-if="!bind.active">
          <span>綁定你的 LINE,行程時間到了才會提醒你。</span>
        </template>
        <template v-else>
          <span>先把 LINE 官方帳號加為好友,再傳送這組綁定碼(10 分鐘內有效):</span>
          <strong class="code" aria-live="polite">{{ prettyCode }}</strong>
          <span class="small">綁定成功後這裡會自動更新。</span>
        </template>
        <p v-if="bind.error" class="err" role="alert">{{ bind.error }}</p>
        <button v-if="!bind.active" type="button" class="link-btn" :disabled="bind.busy" @click="startBind">
          {{ lineBlocked ? '重新綁定 LINE' : '綁定我的 LINE' }}
        </button>
      </div>

      <div v-if="showConnect" class="banner warn" role="alert">
        <span>{{ google.needsReauth ? '需重新授權 Google,授權前無法寫入行事曆。' : '還沒連結 Google 行事曆,連結後才能寫入行程。' }}</span>
        <a class="link-btn" href="/auth/google/calendar">{{ google.needsReauth ? '重新授權 Google' : '連結 Google 行事曆' }}</a>
      </div>

      <!-- Step 1: say or type -->
      <section v-if="!draft" class="compose">
        <label class="sr" for="plan-text">計畫內容</label>
        <div class="box">
          <textarea
            id="plan-text"
            v-model="text"
            rows="4"
            maxlength="1000"
            placeholder="說出或輸入你的計畫,例如:下週三下午兩點帶小孩去看牙醫"
          />
          <button
            v-if="canRecord"
            type="button"
            class="mic"
            :class="{ live: recording }"
            :disabled="transcribing"
            :aria-label="recording ? '停止錄音' : '開始語音輸入'"
            @click="toggleRecord"
          >
            <svg v-if="!recording" viewBox="0 0 24 24" width="22" height="22" aria-hidden="true">
              <path fill="currentColor" d="M12 15a3.5 3.5 0 0 0 3.5-3.5v-6a3.5 3.5 0 1 0-7 0v6A3.5 3.5 0 0 0 12 15Zm6-3.5a1 1 0 1 0-2 0 4 4 0 0 1-8 0 1 1 0 1 0-2 0 6 6 0 0 0 5 5.91V20H9.5a1 1 0 1 0 0 2h5a1 1 0 1 0 0-2H13v-2.59A6 6 0 0 0 18 11.5Z" />
            </svg>
            <svg v-else viewBox="0 0 24 24" width="20" height="20" aria-hidden="true">
              <rect x="6" y="6" width="12" height="12" rx="2" fill="currentColor" />
            </svg>
          </button>
        </div>
        <p class="hint" aria-live="polite">
          <template v-if="recording">錄音中 {{ seconds }} 秒,再按一次結束</template>
          <template v-else-if="transcribing">辨識語音中…</template>
          <template v-else-if="canRecord">按麥克風直接說,說完會轉成文字,可以先修改再解析。</template>
          <template v-else>這個瀏覽器不支援錄音,請直接打字。</template>
        </p>
        <p v-if="error" class="err" role="alert">{{ error }}</p>
        <van-button type="primary" block round :loading="parsing" :disabled="!text.trim() || recording || transcribing" @click="parse">
          解析計畫
        </van-button>
      </section>

      <!-- Step 2: confirm -->
      <section v-else class="confirm">
        <h2>確認計畫</h2>
        <p class="sub">確認無誤才會寫入行事曆。</p>

        <div v-if="draft.ambiguities.length" class="ambig" role="group" aria-label="需要確認的地方">
          <p class="ambig-title">請先確認下列項目,勾選後才能送出</p>
          <label v-for="(a, i) in draft.ambiguities" :key="i" class="check">
            <input v-model="a.ok" type="checkbox" />
            <span>{{ a.text }}</span>
          </label>
        </div>

        <label class="field">標題
          <input v-model="draft.title" type="text" maxlength="200" />
        </label>
        <label class="field" :class="{ need: !draft.start }">開始時間(台北)
          <input v-model="draft.start" type="datetime-local" />
        </label>
        <label class="field">時長(分鐘)
          <span class="row">
            <input v-model.number="draft.duration" type="number" min="1" max="1440" inputmode="numeric" @input="draft.durationIsDefault = false" />
            <em v-if="draft.durationIsDefault" class="tag">預設值</em>
          </span>
        </label>
        <label class="field">類別
          <select v-model="draft.category">
            <option v-for="(name, key) in CATEGORIES" :key="key" :value="key">{{ name }}</option>
          </select>
        </label>
        <label class="field">地點
          <input v-model="draft.location" type="text" maxlength="200" />
        </label>
        <label class="field">備註
          <textarea v-model="draft.notes" rows="2" maxlength="2000" />
        </label>

        <fieldset class="remind">
          <legend>LINE 提醒</legend>
          <template v-if="activeContacts.length">
            <label class="field">提前多久提醒
              <select v-model.number="remindLead">
                <option v-for="o in LEAD_OPTIONS" :key="o.minutes" :value="o.minutes">{{ o.label }}</option>
              </select>
            </label>
            <p class="small">通知誰(預設不通知任何人):</p>
            <div v-if="groups.some((g) => groupMembers(g).length)" class="chips">
              <button
                v-for="g in groups.filter((g) => groupMembers(g).length)"
                :key="g.id"
                type="button"
                class="chip"
                :class="{ on: groupOn(g) }"
                :aria-pressed="groupOn(g)"
                @click="toggleGroup(g)"
              >{{ g.name }}</button>
            </div>
            <label v-for="c in activeContacts" :key="c.id" class="check">
              <input v-model="remindTo" type="checkbox" :value="c.id" />
              <span>{{ c.name }}</span>
            </label>
            <p v-if="remindTo.length" class="small" :class="{ over: overQuota }">
              這次提醒會用 {{ remindTo.length }} 則 LINE 額度<template v-if="remaining !== null">(本月剩 {{ remaining }} 則)</template>。
              <template v-if="overQuota">額度不夠,排在後面的人會收不到。</template>
            </p>
          </template>
          <p v-else class="small">還沒有可以收到提醒的人。先綁定你的 LINE,或到「聯絡人」邀請家人。</p>
        </fieldset>

        <p v-if="error" class="err" role="alert">{{ error }}</p>
        <div class="actions">
          <van-button plain round @click="cancelDraft">返回修改</van-button>
          <van-button type="primary" round :loading="saving" :disabled="!canConfirm" @click="confirm">
            確認並寫入行事曆
          </van-button>
        </div>
      </section>

      <!-- Upcoming -->
      <section class="upcoming">
        <h2>即將到來</h2>
        <p class="small syncline">
          與 Google 日曆同步:{{ syncAgo }}(每 {{ sync.interval }} 分鐘自動檢查)
          <button type="button" class="t" :disabled="sync.busy" @click="syncNow">{{ sync.busy ? '同步中…' : '立即同步' }}</button>
        </p>
        <p v-if="sync.msg" class="small" role="status">{{ sync.msg }}</p>
        <p v-if="syncProblem" class="err" role="alert">{{ syncProblem }}</p>
        <van-empty v-if="!events.length" description="還沒有計畫。" />
        <ul v-else>
          <li v-for="ev in events" :key="ev.id">
            <div>
              <strong>{{ ev.title }}</strong>
              <span class="meta">{{ when(ev.start_time) }} · {{ ev.duration_minutes }} 分鐘<template v-if="ev.location"> · {{ ev.location }}</template></span>
            </div>
            <button type="button" class="del" :aria-label="`刪除 ${ev.title}`" @click="remove(ev)">刪除</button>
          </li>
        </ul>
      </section>

      <p class="who">{{ props.email }}</p>
    </div>
  </div>
</template>

<style scoped>
.home { padding-top: var(--safe-top); padding-bottom: calc(var(--safe-bottom) + 24px); }
.wrap { max-width: 32rem; margin: 0 auto; padding: 8px 16px 0; }
.sr { position: absolute; left: -9999px; }
h2 { font-size: 1.05rem; margin: 28px 0 8px; }
.sub { margin: 0 0 12px; opacity: 0.7; font-size: 0.9rem; }

.banner { border-radius: 10px; padding: 12px 14px; margin: 8px 0 16px; line-height: 1.6; font-size: 0.92rem; }
.banner.ok { background: color-mix(in srgb, var(--ink) 10%, transparent); }
.banner.warn { border-left: 3px solid var(--signal); background: color-mix(in srgb, var(--signal) 10%, transparent);
  display: flex; flex-direction: column; gap: 8px; }
.banner.line { background: color-mix(in srgb, var(--ink) 8%, transparent); display: flex; flex-direction: column; gap: 8px; }
.code { font-size: 1.6rem; letter-spacing: 0.12em; font-variant-numeric: tabular-nums; user-select: all; }
.small { font-size: 0.85rem; opacity: 0.7; margin: 0; }
.small.over { opacity: 1; border-left: 3px solid var(--signal); padding-left: 8px; }
.syncline { margin-bottom: 4px; }
.t { background: none; border: 0; font: inherit; font-size: 0.85rem; color: var(--ink); text-decoration: underline; padding: 8px 6px; cursor: pointer; }
.t:disabled { opacity: 0.5; }
.chips { display: flex; gap: 8px; flex-wrap: wrap; margin: 6px 0; }
.chip { border: 1px solid color-mix(in srgb, var(--ink) 35%, transparent); background: var(--paper); color: var(--ink); border-radius: 999px; padding: 6px 14px; font: inherit; font-size: 0.9rem; cursor: pointer; }
.chip.on { background: var(--ink); color: var(--paper); }
.remind { border: 1px solid color-mix(in srgb, var(--ink) 18%, transparent); border-radius: 10px; margin: 16px 0 0; padding: 4px 12px 10px; }
.remind legend { font-size: 0.85rem; padding: 0 6px; }
button.link-btn { border: 0; font: inherit; cursor: pointer; }
button.link-btn:disabled { opacity: 0.5; }
.link-btn { align-self: flex-start; padding: 6px 14px; border-radius: 999px; background: var(--ink); color: var(--paper);
  text-decoration: none; font-size: 0.9rem; }

.box { position: relative; }
textarea, input, select {
  width: 100%; box-sizing: border-box; font: inherit; font-size: 16px; /* 16px stops iOS zoom-on-focus */
  color: var(--ink); background: color-mix(in srgb, var(--ink) 6%, var(--paper));
  border: 1px solid color-mix(in srgb, var(--ink) 18%, transparent); border-radius: 10px; padding: 10px 12px;
}
.box textarea { padding-right: 64px; resize: none; line-height: 1.6; }
.mic { position: absolute; right: 10px; bottom: 10px; width: 44px; height: 44px; border-radius: 50%;
  border: 1px solid color-mix(in srgb, var(--ink) 30%, transparent); background: var(--paper); color: var(--ink);
  display: grid; place-items: center; cursor: pointer; }
.mic.live { background: var(--signal); border-color: var(--signal); color: #fff; animation: pulse 1.4s ease-in-out infinite; }
.mic:disabled { opacity: 0.5; }
@keyframes pulse { 0%,100% { box-shadow: 0 0 0 0 color-mix(in srgb, var(--signal) 55%, transparent); } 50% { box-shadow: 0 0 0 10px transparent; } }
@media (prefers-reduced-motion: reduce) { .mic.live { animation: none; } }
.hint { font-size: 0.85rem; opacity: 0.7; margin: 8px 0 12px; min-height: 1.3em; }
.err { border-left: 3px solid var(--signal); padding-left: 10px; margin: 8px 0 12px; line-height: 1.6; font-size: 0.92rem; }

.field { display: block; margin: 12px 0; font-size: 0.85rem; }
.field > input, .field > select, .field > textarea { display: block; margin-top: 4px; }
.field.need input { border-color: var(--signal); }
.row { display: flex; align-items: center; gap: 10px; margin-top: 4px; }
.row input { margin: 0; }
.tag { font-style: normal; font-size: 0.75rem; padding: 2px 8px; border-radius: 999px; white-space: nowrap;
  background: color-mix(in srgb, var(--signal) 18%, transparent); }
.ambig { border-left: 3px solid var(--signal); background: color-mix(in srgb, var(--signal) 8%, transparent);
  border-radius: 8px; padding: 10px 12px; margin: 8px 0 12px; }
.ambig-title { margin: 0 0 6px; font-size: 0.85rem; font-weight: 600; }
.check { display: flex; gap: 8px; align-items: flex-start; padding: 6px 0; line-height: 1.5; font-size: 0.92rem; }
.check input { width: 20px; height: 20px; margin: 1px 0 0; padding: 0; flex: none; }
.actions { display: flex; gap: 10px; justify-content: space-between; margin-top: 16px; }
.actions .van-button--primary { flex: 1; }

.upcoming ul { list-style: none; margin: 0; padding: 0; }
.upcoming li { display: flex; justify-content: space-between; align-items: center; gap: 12px; padding: 12px 0;
  border-bottom: 1px solid color-mix(in srgb, var(--ink) 12%, transparent); }
.upcoming strong { display: block; }
.meta { display: block; font-size: 0.85rem; opacity: 0.7; margin-top: 2px; }
.del { background: none; border: 0; color: var(--ink); opacity: 0.7; text-decoration: underline; font: inherit;
  font-size: 0.85rem; padding: 8px; cursor: pointer; }
.who { text-align: center; font-size: 0.8rem; opacity: 0.55; margin: 24px 0 0; }
</style>
