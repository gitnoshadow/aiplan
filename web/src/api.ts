export interface ParseResult {
  title: string
  start_time: string
  duration_minutes: number
  duration_is_default: boolean
  location: string
  notes: string
  category: string
  confidence: number
  ambiguities: string[]
}

export interface EventRow {
  id: number
  title: string
  start_time: string
  duration_minutes: number
  location: string
  notes: string
  category: string
}

export interface Contact {
  id: number
  name: string
  status: 'pending' | 'active' | 'blocked'
  is_self: boolean
  enabled: boolean
}

export interface Group {
  id: number
  name: string
  member_ids: number[]
}

export interface Usage {
  month: string
  sent: number
  limit: number
  level: 'ok' | 'warn' | 'critical' | 'full'
}

export interface ConfirmResult extends EventRow {
  reminder?: 'scheduled' | 'none' | 'event_started' | 'failed'
  reminder_late?: boolean
  reminder_recipients?: number
}

export const LEAD_OPTIONS: { minutes: number; label: string }[] = [
  { minutes: 15, label: '15 分鐘前' },
  { minutes: 30, label: '30 分鐘前' },
  { minutes: 60, label: '1 小時前(預設)' },
  { minutes: 120, label: '2 小時前' },
  { minutes: 1440, label: '1 天前' },
]

export class ApiError extends Error {
  constructor(public code: string, public status: number) {
    super(code)
  }
}

async function call<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, { credentials: 'same-origin', ...init })
  if (res.status === 204) return undefined as T
  let body: any = null
  try {
    body = await res.json()
  } catch {
    /* non-JSON error body */
  }
  if (!res.ok) throw new ApiError(body?.error ?? 'unknown', res.status)
  return body as T
}

const json = (method: string, data: unknown): RequestInit => ({
  method,
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify(data),
})

export const api = {
  googleStatus: () => call<{ connected: boolean; needs_reauth: boolean }>('/api/google/status'),
  parse: (text: string) => call<ParseResult>('/api/plans/parse', json('POST', { text })),
  confirm: (data: Record<string, unknown>) => call<ConfirmResult>('/api/plans/confirm', json('POST', data)),
  contacts: () => call<Contact[]>('/api/contacts'),
  createContact: (name: string) => call<Contact>('/api/contacts', json('POST', { name })),
  updateContact: (id: number, patch: { name?: string; enabled?: boolean }) =>
    call<void>(`/api/contacts/${id}`, json('PATCH', patch)),
  deleteContact: (id: number) => call<void>(`/api/contacts/${id}`, { method: 'DELETE' }),
  invite: (id: number) =>
    call<{ code: string; expires_in_minutes: number; text: string }>(`/api/contacts/${id}/invite`, { method: 'POST' }),
  groups: () => call<Group[]>('/api/groups'),
  saveGroup: (id: number, name: string, memberIds: number[]) =>
    call<Group>(id ? `/api/groups/${id}` : '/api/groups', json(id ? 'PUT' : 'POST', { name, member_ids: memberIds })),
  deleteGroup: (id: number) => call<void>(`/api/groups/${id}`, { method: 'DELETE' }),
  usage: () => call<Usage>('/api/line/usage'),
  bind: () => call<{ code: string; expires_in_minutes: number }>('/api/line/bind', { method: 'POST' }),
  transcribe: (wav: Blob) =>
    call<{ text: string }>('/api/transcribe', { method: 'POST', headers: { 'Content-Type': 'audio/wav' }, body: wav }),
  events: () => call<EventRow[]>('/api/events'),
  deleteEvent: (id: number) => call<void>(`/api/events/${id}`, { method: 'DELETE' }),
}

export const CATEGORIES: Record<string, string> = {
  medical: '就醫',
  meeting: '會議',
  work: '工作',
  school: '學校',
  family: '家庭',
  social: '聚會',
  exercise: '運動',
  errand: '外出辦事',
  travel: '旅行交通',
  reminder: '提醒',
  other: '其他',
}

export const ERROR_TEXT: Record<string, string> = {
  llm_busy: 'Gemini 目前太忙,已自動重試仍未成功。請過一會兒再按一次。',
  too_many_contacts: '聯絡人數量已達上限。',
  invalid_name: '名稱需要 1 到 30 個字。',
  group_failed: '儲存分組失敗,分組名稱可能重複了。',
  cannot_delete_self: '不能刪除自己。',
  parse_failed: '解析失敗,請稍後再試,或換個說法。',
  transcribe_failed: '語音辨識失敗,請再試一次,或直接打字。',
  audio_too_short: '錄音太短,請再說一次。',
  audio_too_large: '錄音太長,請控制在一分鐘內。',
  calendar_failed: '寫入 Google 行事曆失敗,請稍後再試。',
  in_progress: '這筆計畫正在寫入中,請稍候。',
  invalid_plan: '內容有誤,請檢查標題、時間與時長。',
  unauthorized: '登入已過期,請重新整理頁面。',
}
