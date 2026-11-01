# CLAUDE.md — 語音計畫助理(voice-plan-assistant)

> 給日後要修改這個專案的人(或 Claude Code)看的完整說明。先讀完這份再動手。
> 最後更新:v0.8.0。專案七個階段全部完成,使用者確認驗收通過。

## 1. 這是什麼

**只有一個人(管理者)使用**的網頁服務(PWA),加到 iPhone 主畫面使用:

1. 用語音或打字說出計畫 →
2. Gemini 解析成結構化行程 →
3. **確認畫面**(一律要確認才寫入)→
4. 寫入 Google 行事曆(專用的「計畫助理」行事曆)→
5. 時間到了用 **LINE** 提醒管理者與家人,管理者收到完整版(含行前注意事項),家人收到精簡版。

家人**不登入 App**,只是被動接收 LINE 通知的「聯絡人」。不上架任何 App Store,不對外開放註冊。

| 項目 | 值 |
|---|---|
| 部署 | Railway,單一服務 + Postgres,**必須單一實例** |
| 網域 | `aiplan-production-e0e8.up.railway.app` |
| 原始碼 | GitHub 私有 repo `aiplan` |
| LINE 官方帳號 | Bot basic ID `@191xcckr`,加好友連結 `https://line.me/R/ti/p/@191xcckr` |
| 技術棧 | Go 1.23 + Vue 3 + Vite + TypeScript + Vant,Postgres |
| 目前版本 | 見根目錄 `VERSION` 檔,也顯示在畫面底部與 `/api/version` |

## 2. 給 Claude 的工作規則

- **回覆用繁體中文。** 使用者偏好「專業、機智」的語氣。
- **遇到文件沒定義、且會影響架構的地方,先問,不要猜。**
- **每次推送前改 `VERSION`**(例如 `0.7.0` → `0.7.1`)。使用者靠畫面底部的版號判斷「有沒有部署成功」,這是他明確要求的。
- 改程式後跑測試(見第 12 節)。**能離線測的邏輯就寫成不依賴第三方套件的套件**,這是本專案刻意的設計(第 4 節)。
- 時間:資料庫一律存 UTC;顯示與 LLM 解析用 `Asia/Taipei`(固定 UTC+8,見 `plan.Taipei`,不依賴 tzdata)。
- 不要在日誌輸出 token、行程標題/地點/備註全文。錯誤訊息只帶狀態碼或原因代碼(見 `gemini.StatusError`、`line.StatusError`、`gcal.apiErr`)。
- 所有密鑰只放環境變數,不進版本控制。
- 前端目前**不使用任何瀏覽器儲存**(localStorage 等),狀態都在伺服器;要加的話請先確認 PWA 在 iOS 主畫面模式下的行為。

## 3. 架構總覽

```
iPhone PWA (Vue 3)  ──HTTPS──►  Go 服務(單一程序,同時提供前端靜態檔與 API)
                                   │
        ┌──────────────────────────┼───────────────────────────────┐
        ▼                          ▼                               ▼
   Postgres                Google Calendar API            LINE Messaging API
 (Railway 內網)            (OAuth,專用行事曆)          (push / multicast / reply)
                                   ▲                               ▲
                              Gemini API                  LINE 呼叫 /line/webhook
                       (解析計畫、語音轉文字、行前注意事項)     (綁定、封鎖/解除封鎖)

程式內常駐兩個背景迴圈(都在同一個程序):
  • scheduler  每分鐘掃描到期提醒並發送 LINE
  • calsync    每 N 分鐘(預設 5)拉 Google 日曆的變動
```

**為什麼是單一實例:** 排程器在程式內跑。多開會重複發送。資料庫唯一鍵與 LINE retry key 是第二道保險,但不要依賴它。`railway.json` 已設定 `numReplicas: 1`。

## 4. 目錄地圖

後端在 `internal/`,前端在 `web/`。**標 ◇ 的套件只用標準函式庫**,可以在沒有網路的環境直接 `go test`;標 ◆ 的套件依賴 pgx / oauth2 / go-oidc / migrate。

| 路徑 | 職責 |
|---|---|
| `cmd/server/main.go` ◆ | 組裝所有元件、路由、啟動背景迴圈、優雅關閉 |
| `internal/config` ◇ | 讀環境變數並驗證,缺必填就啟動失敗(刻意) |
| `internal/plan` ◇ | **核心領域**:行程結構、`Normalize`(LLM 輸出 → 結果,不猜測)、`DefaultDuration`(各類別預設時長)、解析用 system prompt、`ConfirmInput.Validate` |
| `internal/gemini` ◇ | Gemini REST 客戶端:`ParsePlan`、`Transcribe`、`Tips`;503/429 重試 + 備援模型 |
| `internal/tips` ◇ | **各類別行前注意事項的 prompt**(`Guides` 表)與輸出整理 `Clean` |
| `internal/gcal` ◇ | Google Calendar REST:token 更新、建立專用行事曆、寫入/刪除事件、`ListChanges`(增量同步) |
| `internal/calsync` ◇ | 同步邏輯:`Decide`(遠端變動 → 本地動作,純函式)、`Syncer` 迴圈 |
| `internal/remind` ◇ | 提醒規則:何時發、訊息格式、重試退避、額度等級 |
| `internal/scheduler` ◇ | 發送迴圈:補發、跳過、multicast 合併、額度擋下、封鎖判斷 |
| `internal/line` ◇ | LINE 客戶端、簽章驗證、綁定碼、webhook 處理與猜碼限制 |
| `internal/api` ◇ | 所有 JSON 端點;只依賴介面,所以能用假物件測 |
| `internal/authcore` ◇ | 白名單比對、簽章 cookie、session token(不含 HTTP 與資料庫) |
| `internal/crypto` ◇ | AES-256-GCM,加密 Google refresh token |
| `internal/httpx` ◇ | 健康檢查、SPA 靜態檔服務、日誌/recover/安全標頭 middleware |
| `internal/version` ◇ | 版號、commit、建置時間 |
| `internal/auth` ◆ | Google OIDC 登入(PKCE + nonce)、增量授權行事曆、session 儲存 |
| `internal/data` ◆ | **所有 Postgres 實作**(聯絡人、分組、事件、提醒佇列、同步狀態、額度) |
| `internal/db` ◆ | 連線與內嵌 migration(`migrations/*.sql`) |
| `web/src/App.vue` | 登入頁 / 狀態切換 |
| `web/src/Home.vue` | 主畫面:輸入、錄音、確認畫面、行程列表、同步狀態、LINE 綁定 |
| `web/src/ContactsView.vue` | 聯絡人、邀請、分組、額度 |
| `web/src/recorder.ts` | 麥克風錄音 → 16 kHz 單聲道 WAV |
| `web/src/api.ts` | 後端呼叫與型別、錯誤文字、類別與提醒選項 |
| `web/src/VersionTag.vue` | 畫面底部版號 + 舊版畫面偵測 |
| `web/public/privacy.html` | 隱私權政策(Google 發布應用程式需要) |

**依賴方向:** `api`、`scheduler`、`calsync`、`line` 都只依賴自己定義的介面;`data` 實作這些介面;`main.go` 把它們接起來。要加功能時照這個方式:先在 ◇ 套件寫邏輯與測試,再在 `data` 補 SQL。

## 5. 核心流程

### 5.1 計畫輸入 → 寫入行事曆
1. 前端 `POST /api/plans/parse {text}`。`gemini.ParsePlan` 把**現在的台北時間**、未來 14 天日期對照、「下週X」換算表放進 system prompt,並用 `responseSchema` 約束輸出 JSON。
2. `plan.Normalize`:**不猜測**。缺開始時間、結束時間矛盾、開始時間已過,一律加進 `ambiguities`。沒說時長就套 `DefaultDuration[category]` 並標 `duration_is_default`。
3. 確認畫面:模糊處要**逐項勾選**才能送出;開始時間空白不能送出。
4. `POST /api/plans/confirm`:**這是唯一寫入 Google 的路徑。** 流程是 `Reserve`(以 `request_id` 建立 pending 列,重複請求回傳既有列)→ `gcal.InsertEvent` → `Complete`;Google 失敗就 `Abandon` 並回 409/502,讓使用者可重試。
5. 提醒設定在事件寫入**之後**進行,失敗不會讓確認失敗(事件已在 Google 上),只回 `reminder: "failed"`。

### 5.2 提醒與發送
- 確認時(若有勾收件人)才呼叫 Gemini 產生行前注意事項,**存起來**;發送當下不再呼叫模型,所以 Gemini 忙碌不會漏提醒。類別為 `reminder` 的不產生。
- `remind.Schedule(start, lead, now)`:正常 → 開始前 N 分鐘;提醒時間已過但事件未開始 → **立即發送並標 late**;事件已開始 → 不建立。
- `deliveries` 每列 = (事件, 聯絡人, 提前分鐘),`UNIQUE` 防重複。
- `scheduler.Tick`(每分鐘):撈 `status='pending' AND next_attempt_at <= now`,所以**停機/重啟後會補發**。再依訊息內容分組:同內容多人 → `Multicast`,單人 → `Push`。
- 管理者(`is_self`)收完整版(含 tips),其他人收精簡版(標題、時間、地點、備註;**不含 tips**)。
- 失敗:暫時性錯誤(429/5xx/網路)重試 1、5、15 分鐘後放棄;永久錯誤(其餘 4xx)直接失敗。單人推播遇 400/403/404 才把聯絡人標為「已失效」;**401(我們的 token 壞了)絕不標失效**。
- `X-Line-Retry-Key`(由 delivery id 雜湊成的 UUID)讓「LINE 已收到、我們還沒記下」時當機後的重試不會重複送;LINE 回 409 視為成功。
- **額度**:`line_usage` 按台北月份累計成功發送的則數(每位收件人 1 則)。達 `LINE_MONTHLY_LIMIT` 就不再發(標 `quota_exceeded`),同一組內管理者優先。

### 5.3 LINE 綁定(個人逐人綁定,不做群組)
1. App 產生 8 碼綁定碼(`line.NewBindingCode`:去掉易混字元、拒絕取樣),**資料庫只存雜湊**,10 分鐘有效、單次使用。
2. 對方加官方帳號好友,傳送綁定碼。LINE 呼叫 `/line/webhook`。
3. `Webhook.ServeHTTP`:**先驗 `X-Line-Signature`**(HMAC-SHA256),失敗回 401。
4. `handleText` → `NormalizeCode` → 每個 LINE 帳號 10 分鐘內猜錯 5 次就鎖住(連正確碼也拒)→ `Contacts.Bind`。
5. `Bind` 在交易內 `SELECT ... FOR UPDATE` 鎖住碼那一列,寫入 `line_user_id`、狀態 `active`、標記碼已用。同一個 LINE 帳號只能綁一位聯絡人(唯一索引)。
6. `unfollow` → 聯絡人標 `blocked`;`follow` → 恢復 `active`。群組/聊天室來源的事件一律忽略。回覆訊息用 reply,**不計入推播額度**。

### 5.4 Google 日曆同步(階段 6)
- `gcal.ListChanges`:`singleEvents=true`、`showDeleted=true`;首次(無 token)用 `timeMin = 現在 - 24h` 做完整同步,之後用 `syncToken` 增量。HTTP 410 → `ErrSyncTokenInvalid` → 改做完整重新同步。
- `calsync.Decide` 決定動作:自己寫入的回聲 = `Unchanged`;改時間 = `Update`+重排尚未發的提醒;刪除 = `Cancel`(事件標 `cancelled`、待發提醒標 `skipped`);全日事件 = `Ignore`。
- **token 只在全部套用成功後才存**;失敗會重做(套用是冪等的)。
- 完整重新同步後會 `ReconcileMissing` 取消「Google 已不存在」的未來事件,但若清單裡一個已知事件都沒有(≥3 筆),**拒絕取消**並報錯(視為 API 異常)。
- 只同步**本 App 建立**的事件(以 `google_event_id` 對應),使用者直接在日曆新增的事件不匯入。

### 5.5 語音輸入
iPhone 主畫面 App 裡 Web Speech API 不可靠,所以改成:**手機端錄音編碼成 16 kHz 單聲道 WAV**(`recorder.ts`,需要在使用者點擊時啟動)→ `POST /api/transcribe`(Content-Type `audio/wav`,上限 4 MB)→ Gemini 轉文字 → 填入輸入框讓使用者先修改再解析。後端的 `api.Transcriber` 介面就是原規格的 STT 抽象層,要換別家服務只需實作這個介面。

### 5.6 編輯行程(v0.8.0)
`PATCH /api/events/{id}` 一次送「完整的新內容 + 完整的期望提醒」,不是增量:
1. `plan.ConfirmInput.Validate` 驗證;`Events.Get` 確認屬於本人。
2. **先** `gcal.UpdateEvent`(PATCH)。Google 拒絕就整個中止,本機不動。空的地點/備註送 `null` 才能清掉(省略欄位會留下舊文字)。404/410 → `ErrEventGone`(行程已在 Google 被刪,請同步)。
3. 有勾收件人且行程未開始、類別不是 `reminder` → 重新產生行前注意事項(失敗就保留舊的,`Tips=nil`)。
4. `data.Editor.Apply` 在**單一交易**內:鎖住事件列(與日曆同步共用 `FOR UPDATE`)→ 更新事件 → `remind.Reconcile` 算出要做什麼 → 執行。
5. 若 Google 成功但本機失敗,回 `partial_update`;下一次日曆同步會把 Google 的版本抄回本機,所以兩邊不會長期分歧。

`remind.Reconcile`(純函式,測試最完整)的規則:**已發送的永不重發**;尚未發送的,被取消勾選或提前時間不同者刪除,保留者移到新時間;先前失敗/略過但仍被勾選者復活;沒有紀錄的新收件人新建;事件已開始則什麼都不排、待發的標略過。

## 6. 資料模型(migration 0001–0005)

| 表 | 用途 |
|---|---|
| `users` / `sessions` | 登入者(僅一人)與伺服器端 session(只存 token 雜湊) |
| `google_credentials` | 加密的 refresh token、專用行事曆 ID、狀態 `ok`/`reauth_needed` |
| `events` | 行程:`request_id`(冪等)、`google_event_id`、UTC 開始時間、時長、類別、狀態 `pending`/`active`/`cancelled` |
| `contacts` | 聯絡人:`line_user_id`、狀態 `pending`/`active`/`blocked`、`is_self`、`enabled`(暫停) |
| `binding_codes` | 一次性綁定碼(**只存雜湊**)、到期時間、使用時間 |
| `reminder_rules` | 每事件每提前分鐘一列,存行前注意事項文字 |
| `deliveries` | 發送佇列與紀錄,`UNIQUE(event_id, contact_id, lead_minutes)` 防重複 |
| `line_usage` | 每月(台北時間 `YYYY-MM`)已推送則數 |
| `contact_groups` / `contact_group_members` | 分組,只是確認畫面的快速勾選,與 LINE 群組無關 |
| `calendar_sync` | `sync_token`、最後同步時間、最後錯誤 |

Migration 在每次啟動時自動套用(`db.Migrate`)。**新增 migration 一律往後加編號,不要改舊檔。**

## 7. HTTP 路由

公開(不需登入):`GET /healthz`、`GET /api/version`、`GET /auth/login`、`GET /auth/callback`、`POST /auth/logout`、`POST /line/webhook`(靠簽章驗證)、靜態檔與 SPA。

需登入(session cookie,未登入回 401):

| 路由 | 用途 |
|---|---|
| `GET /api/me` | 目前登入者 |
| `GET /auth/google/calendar` | 增量授權行事曆(導向 Google) |
| `GET /api/google/status` | 是否已連結、是否需重新授權 |
| `POST /api/plans/parse` | 解析(**不寫入**) |
| `POST /api/plans/confirm` | 確認並寫入 Google + 排提醒 |
| `POST /api/transcribe` | 語音轉文字 |
| `GET /api/events`、`PATCH /api/events/{id}`、`DELETE /api/events/{id}` | 列表(每筆附 `reminders`:提前多久、通知誰、各人發送狀態,由 `api.GroupDeliveries` 組出)、編輯(內容 + 提醒,見 5.6)、刪除(同步刪 Google) |
| `GET/POST /api/contacts`、`PATCH/DELETE /api/contacts/{id}` | 聯絡人管理(不能刪自己)。`GET` 對已綁定者附上 `line_user_id`(只在登入後提供,前端預設遮罩顯示) |
| `POST /api/contacts/{id}/invite` | 產生綁定碼與可轉傳的邀請文字 |
| `POST /api/line/bind` | 管理者自己綁定 LINE |
| `GET/POST /api/groups`、`PUT/DELETE /api/groups/{id}` | 分組 |
| `GET /api/line/usage` | 本月額度與等級(`ok`/`warn` 80%/`critical` 95%/`full`) |
| `GET /api/sync/status`、`POST /api/sync/now` | 日曆同步狀態、立即同步 |

## 8. 環境變數

必填(缺任何一個服務啟動失敗):

| 變數 | 說明 |
|---|---|
| `DATABASE_URL` | Railway:填參照變數 `${{Postgres.DATABASE_URL}}`,**不要手貼、不要留範例值** |
| `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` | Google OAuth 用戶端(網頁應用程式) |
| `OAUTH_REDIRECT_URL` | `https://<網域>/auth/callback`,要和 Google 後台逐字相同 |
| `APP_BASE_URL` | `https://<網域>`,**結尾不加斜線**;`https://` 會開啟 Secure cookie |
| `ALLOWED_EMAIL` | 唯一允許登入的 Google 帳號 |
| `SESSION_SECRET` | ≥ 32 字元亂數 |
| `ENCRYPTION_KEY` | base64 的 32 位元組(`openssl rand -base64 32`)。**換掉會讓已存的 refresh token 失效** |
| `GEMINI_API_KEY` / `LLM_MODEL` | 模型名稱不寫死;目前用 `gemini-3.7-flash` |
| `LINE_CHANNEL_SECRET` / `LINE_CHANNEL_ACCESS_TOKEN` | LINE Developers Console |

選填:`LLM_FALLBACK_MODEL`(主模型持續忙碌時改用)、`LINE_MONTHLY_LIMIT`(預設 200,0 = 不限制)、`LINE_ADD_FRIEND_URL`(邀請文字用)、`CALENDAR_SYNC_MINUTES`(預設 5,範圍 1–60)、`PORT`(Railway 自動給)、`STATIC_DIR`(Docker 已設)。`RAILWAY_GIT_COMMIT_SHA` 由 Railway 自動帶入,用於版號顯示。

## 9. 外部服務設定(從零重建時照做)

**Google Cloud**
1. 建專案 → Google Auth Platform:設定品牌(名稱、支援信箱)、目標對象「外部」。
2. 啟用 **Google Calendar API**。
3. 建 OAuth 用戶端(網頁應用程式),已授權重新導向 URI 填 `https://<網域>/auth/callback`。
4. 目標對象 → 測試使用者加入自己的 Gmail。
5. **發布為正式版**需要在「品牌」填**應用程式首頁**與**隱私權政策連結**(本專案提供 `/privacy.html`)。測試狀態下 refresh token 只有約 7 天壽命,到期首頁會出現「重新授權 Google」按鈕,按一下即可。未驗證的正式版登入時會有警告,點「進階 → 前往」。
   > 目前是否已發布為正式版,請以 Google Cloud 後台為準。

**LINE Developers Console**(Messaging API 頻道)
1. Basic settings 的 Channel secret、Messaging API 的 Channel access token(長期)→ Railway 變數。
2. **先部署,再填** Webhook URL `https://<網域>/line/webhook`,開啟 Use webhook,按 Verify。
3. **關閉自動回應訊息**(否則每則來訊都多耗額度)。
4. 免費方案每月 200 則,不能加購;每位收件人算一則。

**Railway**
- 專案內一個 App 服務(接 GitHub repo `aiplan`)+ 一個 Postgres。建置用 `Dockerfile`。
- **不要刪 App 服務**:網域綁在服務上,重建會換網址,Google 的 redirect URI、品牌頁網址、`APP_BASE_URL`、`OAUTH_REDIRECT_URL` 都要跟著改。

**Gemini**:Google AI Studio 取得金鑰。使用者的計畫文字與錄音會送到 Gemini;免費層的資料可能被用於改進產品,付費層通常不會,以 Google 現行條款為準。

## 10. 重要設計決策與原因

- **專用行事曆 + `calendar.app.created` 範圍。** 一開始用 `calendar.events`,結果建立行事曆那一步回 403:這個範圍**不能建立行事曆**。`calendar.app.created` 讓 App 能建立自己的行事曆並只管理其中的事件,碰不到使用者的主要行事曆,權限反而更小。**不要換回 `calendar.events`。**
- **確認畫面是唯一寫入路徑**,且有 `request_id` 冪等:連按兩次只會有一筆 Google 事件。
- **登入與行事曆授權分開、增量授權**:登入只要 `openid email`;行事曆另走 `/auth/google/calendar`(`access_type=offline` + `prompt=consent` 才拿得到 refresh token),同一個 callback 用簽章 cookie 裡的 `Purpose` 區分。授權完會檢查使用者有沒有取消勾選行事曆範圍。
- **行前注意事項在確認時產生並儲存**,不在發送時才呼叫模型。
- **同內容才合併 multicast**:管理者與家人內容不同,所以各自發送;多位家人內容相同才併成一次 multicast。
- **Gemini 輸出雙重保護**:`responseSchema` 約束格式,伺服器再用 `plan.Normalize` / `tips.Clean` 驗證與整理,不信任模型。
- **LLM 重試只針對暫時性錯誤**(429/5xx),永久錯誤(400/401/403/404)不重試。錯誤只記狀態碼,因為回應內容可能回顯使用者的計畫。
- **Gemini 3 系列預設會「思考」,思考 token 算在 `maxOutputTokens` 內**,所以上限設 8192(解析)/ 4096(其他);設太小 JSON 會被截斷。
- **Session 存在資料庫、cookie 只放隨機 token**(HttpOnly、SameSite=Lax、HTTPS 時 Secure),資料庫只存雜湊。
- **版號三重來源**:`VERSION` 檔同時寫進前端(`VITE_APP_VERSION`)與後端(`-ldflags -X`),再加上 Railway 的 commit。畫面與伺服器版本不同時會提示「畫面是舊版」,因為主畫面 App 可能卡在舊頁面。
- **邏輯放進不依賴第三方套件的套件**(`plan`、`remind`、`scheduler`、`calsync`、`tips`…),因為撰寫這個專案的沙盒抓不到 Go 套件,只有這些能實際編譯與測試。

## 11. 踩過的坑(下次不用再踩)

| 症狀 | 原因與解法 |
|---|---|
| 服務啟動就掛,日誌 `missing required env vars` | 少了環境變數。這是刻意的 fail-fast |
| 日誌連到 `user:password@localhost:5432` | `DATABASE_URL` 還是 `.env.example` 的範例值。改成參照 Postgres 服務 |
| 「Application failed to respond」 | 程式啟動失敗,看 Deploy Logs |
| 「The train has not arrived at the station」 | Railway 網域不存在(重建服務後網域會變)。到 Settings → Networking 重新產生,並同步更新 Google 與環境變數 |
| Google 後台「發布應用程式」是灰的 | 缺應用程式首頁與隱私權政策網址 |
| 登入時 `redirect_uri_mismatch` | Google 後台 URI 與 `OAUTH_REDIRECT_URL` 沒有逐字相同 |
| 寫入行事曆 `create calendar: status 403` | 範圍用成 `calendar.events`(見第 10 節);或沒啟用 Calendar API |
| 解析失敗,日誌 `gemini ... status 503` | Gemini 過載。已內建重試與選填的備援模型;也可改 `LLM_MODEL` |
| 解析 JSON 被截斷 | Gemini 3 思考 token 吃掉輸出上限,見第 10 節 |
| 換模型要改哪裡 | 只改 Railway 的 `LLM_MODEL`,不用改程式 |
| `pnpm` 安裝在 Docker 內失敗 | 需要 `web/pnpm-workspace.yaml`(`allowBuilds: esbuild: true`),且 Dockerfile 要一起複製它 |
| Vue 範本裡 `location.reload()` 型別錯誤 | 範本裡的 `location` 不是瀏覽器全域,要在 script 裡用函式包起來 |
| 區域變數 `tips` 蓋掉同名套件 | 改名(`advice`)。Go 的 shadowing 錯誤訊息很不直覺 |
| LINE 每則訊息都多一則自動回覆 | 沒關「自動回應訊息」 |
| LINE Webhook Verify 失敗 | 在程式部署**之前**就按了。先部署再驗證 |
| 誤刪 GitHub repo | 本機資料夾還在就重新 `git init` 推上去;Railway 重新連結 repo,**不要刪服務** |
| iPhone 語音辨識不穩 | Web Speech API 在主畫面 App 不可靠,所以改走 WAV 錄音 + Gemini |
| 移除 Google 第三方存取後無法寫入 | 授權已被撤銷。直接開 `/auth/google/calendar` 重新連結(App 認為已連結時首頁不會出現按鈕) |

## 12. 測試與驗證

```bash
# 只依賴標準函式庫的套件(任何環境都能跑)
go test ./internal/plan ./internal/gemini ./internal/tips ./internal/gcal ./internal/calsync \
        ./internal/remind ./internal/scheduler ./internal/line ./internal/api ./internal/config \
        ./internal/version ./internal/authcore ./internal/crypto ./internal/httpx

# 全部(需要能下載 Go 套件;第一次先 `go mod tidy` 並提交 go.sum)
go test ./...

# 含資料庫整合測試(請用拋棄式資料庫)
TEST_DATABASE_URL=postgresql://... go test ./internal/auth/

# 前端
cd web && pnpm install && pnpm build   # 含 vue-tsc 型別檢查
```

**驗證狀態要誠實看待:**
- ◇ 套件:都有單元測試,且實際跑過。
- **`internal/data`、`internal/auth`、`internal/db`、`cmd/server` 從未在撰寫環境編譯過**(抓不到 pgx/oauth2 等套件),只做過語法檢查與介面對照,正確性是靠使用者實際部署與驗收確認的。**日後若這些地方出問題,先懷疑 SQL 與型別。**
- 真實的 Google / Gemini / LINE 回應,只在使用者的實際環境驗證過。
- `go.sum` 沒有隨專案提供,Docker 建置時會跑 `go mod tidy`。有 Go 環境時請執行一次並提交。

**型別檢查資料層的小技巧(沒有網路抓 pgx 時):** 建一個暫存模組,把 `internal/` 複製進去,用一個只含 `pgx.Row/Rows/Tx`、`pgxpool.Pool`、`pgconn.CommandTag/PgError` 的假 `github.com/jackc/pgx/v5`(`go.mod` 的 `replace`)當替身,就能對 `internal/data` 做真正的 `go vet`,抓得到型別與介面不符。v0.8.0 起 `internal/data` 用這個方法檢查過,但**仍不是真實資料庫上的測試**,SQL 是否正確只有實際部署才知道。

各版本的驗收清單在 `README.md`。

## 13. 常見修改配方

| 想做的事 | 改哪裡 |
|---|---|
| 調整各類別預設時長 | `internal/plan/plan.go` 的 `DefaultDuration` |
| 調整行前注意事項的重點/條數/禁忌 | `internal/tips/tips.go` 的 `Guides` 表 |
| **新增行程類別** | `plan.Categories`、`plan.DefaultDuration`、`tips.Guides`、`tips.labels`、前端 `web/src/api.ts` 的 `CATEGORIES`(測試會檢查前三者同步) |
| 改提醒訊息的版面 | `internal/remind/remind.go` 的 `Format` |
| 改「提前多久」選項 / 預設 | 前端 `LEAD_OPTIONS`(`api.ts`);預設值 `remind.DefaultLeadMinutes` 與 `Home.vue` 的 `remindLead` |
| 改重試次數與間隔 | `remind.backoff` 與 `remind.MaxAttempts` |
| 換 LLM 模型 / 備援 | Railway 變數 `LLM_MODEL`、`LLM_FALLBACK_MODEL` |
| 換額度上限 | `LINE_MONTHLY_LIMIT` |
| 改同步頻率 | `CALENDAR_SYNC_MINUTES` |
| 解析規則(怎麼理解「下週三」、模糊處理) | `plan.SystemPrompt` 與 `plan.Normalize` |
| 改「編輯時提醒怎麼跟著變」的規則 | `internal/remind/reconcile.go` 的 `Reconcile`(先改測試 `reconcile_test.go`),SQL 執行端在 `internal/data/edit.go` |
| 新增 API 端點 | 在 `internal/api/api.go` 加介面方法與處理函式、在 `Register` 掛路由、`data` 補實作、`main.go` 接線、`api_test.go` 補測試 |
| 新增資料表/欄位 | 新增 `internal/db/migrations/000N_*.up.sql` 與 `.down.sql` |
| 換語音辨識服務 | 實作 `api.Transcriber` 介面,在 `main.go` 換掉 `llm` |
| 前端換主題色 | `web/src/style.css` 的 CSS 變數 |

## 14. 部署流程

1. 改 `VERSION`。
2. 本機(有 Go 與 pnpm 時)跑第 12 節的測試與 `pnpm build`。
3. `git add . && git commit && git push`,Railway 自動建置部署。
4. 看畫面底部的版號與 7 碼 commit,或開 `https://<網域>/api/version`。
5. 開 `/healthz` 應回 `ok`。
6. 依 `README.md` 的對應驗收清單測。

解壓縮 Claude 給的更新檔時:`tar -xzf <檔名> --strip-components=1`(會覆蓋同名檔,不會動 `.git`)。

## 15. 已知限制與可能的後續

- 只同步**本 App 建立**的事件;使用者直接在 Google 日曆新增的事件不匯入、沒有提醒。
- 已發出的提醒不會因為改期而重發。
- 改成全日事件、改成重複事件,不會被追蹤。
- 多人 multicast 時,LINE 對已封鎖的收件人通常不回報錯誤,封鎖要等 `unfollow` 事件才會被偵測到。
- 單人使用的設計:資料表保留 `user_id`,但登入白名單只有一個帳號,多使用者需要重新設計授權與 LINE 通道。
- 不支援重複性計畫的自然語言建立(只做讀取與提醒)、多時區(固定台北)。
- 未做:天氣/交通時間整合(`tips` 與 `remind` 是擴充點)、LINE 群組推播、家人以 LINE 對 App 下指令。
- 可改進:`TEST_DATABASE_URL` 整合測試目前只涵蓋 session,`data` 層其餘查詢沒有自動化測試。

## 16. 開發時間線

| 交付名稱 | 階段 | 重點 |
|---|---|---|
| stage 1 | 1 骨架與登入 | Go + Vue + Vant、Google 登入(PKCE)、單一帳號白名單、session、Railway 部署 |
| stage 2 | 2 文字輸入 → 寫入行事曆 | Gemini 解析、確認畫面、專用行事曆、token 加密、冪等寫入;**麥克風(WAV)一併完成**;原規格的 Anthropic 改用 Gemini |
| stage 2 修正 | — | Gemini 503 重試與備援模型、思考 token 上限、行事曆範圍改 `calendar.app.created` |
| stage 3 | 3 LINE 通知自己 | webhook 簽章、綁定碼、提醒規則、排程器、`deliveries` 防重複、補發與降級 |
| stage 4 | 4 聯絡人與多人發送 | 聯絡人管理、家人邀請、分組、multicast、精簡版、額度顯示與擋下、失效處理 |
| v0.4.1 | — | 畫面底部顯示版號與 commit(使用者要求,用來確認部署成功)。**從這一版起才有 `VERSION` 檔與版號命名** |
| v0.6.0 | 6 日曆增量同步 | syncToken、改期重排提醒、刪除取消提醒、立即同步 |
| v0.7.0 | 7 依類別客製注意事項 | `tips.Guides`、依距離開始時間調整、輸出整理、`reminder` 類別不產生 |
| v0.7.1 | — | 聯絡人頁顯示已綁定者的 LINE userId(遮罩、可展開與複製) |
| v0.7.2 | — | 行程列表顯示「提前多久、通知誰、各人發送狀態」 |
| v0.8.0 | — | App 內編輯行程:改內容、提前時間、通知對象;`remind.Reconcile`、`gcal.UpdateEvent`、`data.Editor` |

階段 5(STT 抽象介面)沒有獨立版本:語音輸入在階段 2 就做完,`api.Transcriber` 介面即是原規格的抽象層。

## 17. 規格決策紀錄(來自使用者的原始提示詞與後續回覆)

- 主機 Railway;排程用程式內常駐(不用 Railway Cron,其最短間隔 5 分鐘太粗);行事曆用 Google Calendar(Tasks 的 due 沒有時間);通知用 LINE Messaging API(LINE Notify 已終止服務)。
- 收件方式:逐人綁定,第一版**不做群組推播**。
- 收件人預設為空,每次由使用者勾選(避免敏感行程外流)。
- 提醒預設提前 1 小時;提醒時間已過 → 立即提醒並註明;事件已開始 → 一律跳過。
- 未說明時長 → 依類別給預設值,並在確認畫面標示「預設值」。
- 家人收到的精簡版**包含備註**,行前注意事項完整版只有使用者本人收到。
- 額度:預設每月 200 則,80% 黃色提示、95% 紅色警示,達上限前擋下。
- 天氣與交通:第一版不做,只預留擴充點。
- 使用者的 UI 偏好:要用現成元件庫(Vant),不要純 CSS。
