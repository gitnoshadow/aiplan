<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref } from 'vue'
import { api, ApiError, ERROR_TEXT, type Contact, type Group, type Usage } from './api'

const props = defineProps<{ contacts: Contact[]; groups: Group[]; usage: Usage | null }>()
const emit = defineEmits<{ (e: 'back'): void; (e: 'changed'): void }>()

const error = ref('')
const fail = (e: unknown) => {
  error.value = e instanceof ApiError ? (ERROR_TEXT[e.code] ?? '發生錯誤,請再試一次。') : '發生錯誤,請再試一次。'
}

// ---- usage -----------------------------------------------------------------
const pct = computed(() => (props.usage && props.usage.limit > 0 ? Math.min(100, Math.round((props.usage.sent / props.usage.limit) * 100)) : 0))
const usageNote = computed(() => {
  switch (props.usage?.level) {
    case 'warn': return '額度已用超過 80%,請留意。'
    case 'critical': return '額度快用完了(95% 以上)。用完後提醒會停止發送。'
    case 'full': return '本月額度已用完,提醒不會再發送,下個月自動恢復。'
    default: return ''
  }
})

// ---- contacts --------------------------------------------------------------
const STATUS: Record<string, string> = { pending: '待綁定', active: '已啟用', blocked: '已失效' }
const newName = ref('')
const editing = reactive<{ id: number; name: string }>({ id: 0, name: '' })

async function addContact() {
  error.value = ''
  try {
    await api.createContact(newName.value)
    newName.value = ''
    emit('changed')
  } catch (e) { fail(e) }
}
function startRename(c: Contact) { editing.id = c.id; editing.name = c.name }
async function saveRename() {
  const id = editing.id
  editing.id = 0
  try { await api.updateContact(id, { name: editing.name }); emit('changed') } catch (e) { fail(e) }
}
async function toggle(c: Contact) {
  try { await api.updateContact(c.id, { enabled: !c.enabled }); emit('changed') } catch (e) { fail(e) }
}
async function remove(c: Contact) {
  if (!window.confirm(`刪除「${c.name}」?他的 LINE 綁定與相關提醒紀錄會一併清除。`)) return
  try { await api.deleteContact(c.id); emit('changed') } catch (e) { fail(e) }
}

// ---- invitation ------------------------------------------------------------
const invite = reactive({ id: 0, text: '', copied: false })
let poll: number | undefined
const stopPoll = () => { window.clearInterval(poll); poll = undefined }

async function startInvite(c: Contact) {
  error.value = ''
  try {
    const r = await api.invite(c.id)
    invite.id = c.id
    invite.text = r.text
    invite.copied = false
    stopPoll()
    let n = 0
    poll = window.setInterval(() => {
      emit('changed') // the parent refreshes contacts; we watch for the status change below
      if (++n > r.expires_in_minutes * 20) stopPoll()
    }, 3000)
  } catch (e) { fail(e) }
}
const inviteTarget = computed(() => props.contacts.find((c) => c.id === invite.id))
const inviteDone = computed(() => inviteTarget.value?.status === 'active')
function closeInvite() { stopPoll(); invite.id = 0 }
async function copy() {
  try { await navigator.clipboard.writeText(invite.text); invite.copied = true } catch { error.value = '無法自動複製,請長按文字手動複製。' }
}
async function share() {
  try { await navigator.share?.({ text: invite.text }) } catch { /* user cancelled */ }
}
const canShare = typeof navigator !== 'undefined' && !!navigator.share
onBeforeUnmount(stopPoll)

// ---- groups ----------------------------------------------------------------
interface Draft { id: number; name: string; members: number[] }
const drafts = ref<Draft[]>([])
const groupsView = computed<Draft[]>(() => [
  ...props.groups.filter((g) => !drafts.value.some((d) => d.id === g.id)).map((g) => ({ id: g.id, name: g.name, members: [...g.member_ids] })),
  ...drafts.value,
])
function addGroup() { drafts.value.push({ id: 0, name: '', members: [] }) }
async function saveGroup(g: Draft) {
  error.value = ''
  try {
    await api.saveGroup(g.id, g.name, g.members)
    drafts.value = drafts.value.filter((d) => d !== g)
    emit('changed')
  } catch (e) { fail(e) }
}
async function removeGroup(g: Draft) {
  if (!g.id) { drafts.value = drafts.value.filter((d) => d !== g); return }
  if (!window.confirm(`刪除分組「${g.name}」?聯絡人不會被刪除。`)) return
  try { await api.deleteGroup(g.id); emit('changed') } catch (e) { fail(e) }
}
</script>

<template>
  <div class="cv">
    <van-nav-bar title="聯絡人與額度">
      <template #left><van-button size="small" plain @click="emit('back')">返回</van-button></template>
    </van-nav-bar>

    <div class="wrap">
      <p v-if="error" class="err" role="alert">{{ error }}</p>

      <section v-if="usage">
        <h2>本月 LINE 額度</h2>
        <p class="big">已發送 {{ usage.sent }}<template v-if="usage.limit"> / {{ usage.limit }}</template> 則</p>
        <div v-if="usage.limit" class="bar" :class="usage.level" role="progressbar" :aria-valuenow="pct" aria-valuemin="0" aria-valuemax="100">
          <span :style="{ width: pct + '%' }" />
        </div>
        <p v-if="usageNote" class="note" :class="usage.level">{{ usageNote }}</p>
        <p class="small">每位收件人算一則,多人提醒會消耗得更快。</p>
      </section>

      <section>
        <h2>聯絡人</h2>
        <ul class="list">
          <li v-for="c in contacts" :key="c.id" :class="{ off: !c.enabled }">
            <div class="row1">
              <template v-if="editing.id === c.id">
                <input v-model="editing.name" maxlength="30" @keyup.enter="saveRename" />
                <button type="button" class="t" @click="saveRename">儲存</button>
              </template>
              <template v-else>
                <strong>{{ c.name }}</strong>
                <em class="tag" :class="c.status">{{ STATUS[c.status] }}</em>
                <em v-if="!c.enabled" class="tag paused">已暫停</em>
              </template>
            </div>
            <div class="row2">
              <button v-if="!c.is_self" type="button" class="t" @click="startRename(c)">改名</button>
              <button v-if="!c.is_self && c.status !== 'active'" type="button" class="t" @click="startInvite(c)">產生邀請</button>
              <button type="button" class="t" @click="toggle(c)">{{ c.enabled ? '暫停' : '恢復' }}</button>
              <button v-if="!c.is_self" type="button" class="t danger" @click="remove(c)">刪除</button>
            </div>
            <p v-if="c.status === 'blocked'" class="small">對方封鎖了官方帳號或無法接收,請對方解除封鎖後重新綁定。</p>

            <div v-if="invite.id === c.id" class="invite">
              <template v-if="inviteDone">
                <p class="ok">「{{ c.name }}」綁定成功。</p>
                <button type="button" class="t" @click="closeInvite">關閉</button>
              </template>
              <template v-else>
                <p class="small">把下面這段話傳給對方。對方完成後,這裡會自動更新。</p>
                <textarea :value="invite.text" readonly rows="6" />
                <div class="row2">
                  <button type="button" class="pill" @click="copy">{{ invite.copied ? '已複製' : '複製' }}</button>
                  <button v-if="canShare" type="button" class="pill" @click="share">分享</button>
                  <button type="button" class="t" @click="closeInvite">關閉</button>
                </div>
              </template>
            </div>
          </li>
        </ul>
        <form class="add" @submit.prevent="addContact">
          <input v-model="newName" maxlength="30" placeholder="新增聯絡人,例如:媽媽" />
          <button type="submit" class="pill" :disabled="!newName.trim()">新增</button>
        </form>
      </section>

      <section>
        <h2>分組</h2>
        <p class="small">分組只是讓你在確認畫面一次勾選多人,和 LINE 群組無關。</p>
        <div v-for="(g, i) in groupsView" :key="g.id || 'new' + i" class="group">
          <input v-model="g.name" maxlength="30" placeholder="分組名稱,例如:家人" />
          <label v-for="c in contacts" :key="c.id" class="check">
            <input v-model="g.members" type="checkbox" :value="c.id" />
            <span>{{ c.name }}</span>
          </label>
          <div class="row2">
            <button type="button" class="pill" :disabled="!g.name.trim()" @click="saveGroup(g)">儲存</button>
            <button type="button" class="t danger" @click="removeGroup(g)">刪除</button>
          </div>
        </div>
        <button type="button" class="pill" @click="addGroup">新增分組</button>
      </section>
    </div>
  </div>
</template>

<style scoped>
.cv { padding-top: var(--safe-top); padding-bottom: calc(var(--safe-bottom) + 24px); }
.wrap { max-width: 32rem; margin: 0 auto; padding: 8px 16px 0; }
h2 { font-size: 1.05rem; margin: 28px 0 8px; }
.small { font-size: 0.85rem; opacity: 0.7; margin: 6px 0; }
.big { font-size: 1.3rem; margin: 4px 0 8px; font-variant-numeric: tabular-nums; }
.err { border-left: 3px solid var(--signal); padding-left: 10px; margin: 8px 0; line-height: 1.6; font-size: 0.92rem; }
.bar { height: 10px; border-radius: 999px; background: color-mix(in srgb, var(--ink) 12%, transparent); overflow: hidden; }
.bar span { display: block; height: 100%; background: var(--ink); }
.bar.warn span { background: #d9a400; }
.bar.critical span, .bar.full span { background: var(--signal); }
.note { margin: 8px 0 0; padding-left: 10px; border-left: 3px solid #d9a400; font-size: 0.9rem; }
.note.critical, .note.full { border-color: var(--signal); }
.list { list-style: none; margin: 0; padding: 0; }
.list li { padding: 12px 0; border-bottom: 1px solid color-mix(in srgb, var(--ink) 12%, transparent); }
.list li.off strong { opacity: 0.5; }
.row1 { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.row2 { display: flex; gap: 6px; flex-wrap: wrap; margin-top: 6px; align-items: center; }
.tag { font-style: normal; font-size: 0.75rem; padding: 2px 8px; border-radius: 999px; background: color-mix(in srgb, var(--ink) 12%, transparent); }
.tag.active { background: color-mix(in srgb, #2a9d62 25%, transparent); }
.tag.blocked { background: color-mix(in srgb, var(--signal) 25%, transparent); }
.t { background: none; border: 0; font: inherit; font-size: 0.85rem; color: var(--ink); text-decoration: underline; padding: 8px 6px; cursor: pointer; }
.t.danger { color: var(--signal); }
.pill { border: 1px solid color-mix(in srgb, var(--ink) 35%, transparent); background: var(--paper); color: var(--ink); border-radius: 999px; padding: 8px 16px; font: inherit; font-size: 0.9rem; cursor: pointer; }
.pill:disabled { opacity: 0.45; }
input, textarea { width: 100%; box-sizing: border-box; font: inherit; font-size: 16px; color: var(--ink); background: color-mix(in srgb, var(--ink) 6%, var(--paper)); border: 1px solid color-mix(in srgb, var(--ink) 18%, transparent); border-radius: 10px; padding: 10px 12px; }
.add { display: flex; gap: 8px; margin-top: 12px; }
.invite { margin-top: 10px; padding: 10px 12px; border-radius: 10px; background: color-mix(in srgb, var(--ink) 6%, transparent); }
.invite textarea { resize: none; line-height: 1.6; margin-top: 6px; }
.ok { margin: 0; font-weight: 600; }
.group { margin: 12px 0; padding: 10px 12px; border: 1px solid color-mix(in srgb, var(--ink) 18%, transparent); border-radius: 10px; }
.check { display: flex; gap: 8px; align-items: center; padding: 6px 0; font-size: 0.92rem; }
.check input { width: 20px; height: 20px; padding: 0; flex: none; }
</style>
