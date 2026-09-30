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
}

export interface ConfirmResult extends EventRow {
  reminder?: 'scheduled' | 'none' | 'event_started' | 'failed'
  reminder_late?: boolean
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
  parse_failed: '解析失敗,請稍後再試,或換個說法。',
  transcribe_failed: '語音辨識失敗,請再試一次,或直接打字。',
  audio_too_short: '錄音太短,請再說一次。',
  audio_too_large: '錄音太長,請控制在一分鐘內。',
  calendar_failed: '寫入 Google 行事曆失敗,請稍後再試。',
  in_progress: '這筆計畫正在寫入中,請稍候。',
  invalid_plan: '內容有誤,請檢查標題、時間與時長。',
  unauthorized: '登入已過期,請重新整理頁面。',
}
