<script setup lang="ts">
import { onMounted, ref } from 'vue'
import Home from './Home.vue'
import VersionTag from './VersionTag.vue'

type State = 'loading' | 'anon' | 'authed'
const state = ref<State>('loading')
const email = ref('')
const errorText = ref('')
const notice = ref('')

const ERRORS: Record<string, string> = {
  forbidden: '這個 Google 帳號沒有使用權限。請改用指定的帳號登入。',
  failed: '登入沒有完成,請再試一次。',
  calendar_scope: '沒有勾選行事曆權限,無法寫入行程。請重新連結,並勾選「查看、編輯、分享及永久刪除您可透過 Google 日曆存取的所有日曆」的相關項目。',
  calendar_failed: '連結 Google 行事曆失敗,請再試一次。',
}

async function loadMe() {
  try {
    const res = await fetch('/api/me', { credentials: 'same-origin' })
    if (res.ok) {
      email.value = (await res.json()).email
      state.value = 'authed'
      return
    }
  } catch {
    errorText.value = '無法連線到伺服器。'
  }
  state.value = 'anon'
}

async function logout() {
  await fetch('/auth/logout', { method: 'POST', credentials: 'same-origin' })
  email.value = ''
  state.value = 'anon'
}

onMounted(() => {
  const params = new URLSearchParams(location.search)
  const code = params.get('error')
  if (code) errorText.value = ERRORS[code] ?? ERRORS.failed
  if (params.get('calendar') === 'connected') notice.value = '已連結 Google 行事曆,之後的行程會寫入「計畫助理」行事曆。'
  if (code || params.has('calendar')) history.replaceState(null, '', '/')
  loadMe()
})
</script>

<template>
  <main v-if="state === 'anon'" class="login">
    <div class="login-body">
      <h1>語音計畫助理</h1>
      <p class="lead">說出你的計畫,確認後寫進 Google 行事曆,時間到了用 LINE 提醒你和家人。</p>
      <p v-if="errorText" class="error" role="alert">{{ errorText }}</p>
    </div>
    <van-button type="primary" block round size="large" tag="a" href="/auth/login">
      使用 Google 登入
    </van-button>
  </main>

  <template v-else-if="state === 'authed'">
    <p v-if="errorText" class="error top" role="alert">{{ errorText }}</p>
    <Home :email="email" :notice="notice" @logout="logout" />
  </template>

  <VersionTag v-if="state !== 'loading'" />
</template>

<style scoped>
.login {
  min-height: 100%;
  box-sizing: border-box;
  display: flex;
  flex-direction: column;
  justify-content: space-between;
  padding: calc(var(--safe-top) + 20vh) 24px calc(var(--safe-bottom) + 32px);
}
.login-body { max-width: 20rem; }
h1 { font-size: 2rem; line-height: 1.25; margin: 0 0 1rem; letter-spacing: 0.02em; }
.lead { font-size: 1rem; line-height: 1.7; margin: 0; opacity: 0.8; }
.error {
  margin: 1.5rem 0 0;
  padding-left: 0.75rem;
  border-left: 3px solid var(--signal);
  line-height: 1.6;
}
.top { margin: calc(var(--safe-top) + 12px) 16px 0; }
</style>
