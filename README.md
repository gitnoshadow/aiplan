# 語音計畫助理

Go + Vue 3 (Vant) 單一服務。階段 1:骨架與 Google 登入(單一帳號白名單)。階段 2:文字或語音輸入 → Gemini 解析 → 確認畫面 → 寫入專用 Google 行事曆。階段 3:LINE 提醒(綁定、排程器、防重複發送)。階段 4:聯絡人、家人邀請、分組、多人發送、額度顯示。

## 本機開發

```bash
cp .env.example .env        # 填好後用 `set -a; . ./.env; set +a` 載入
go mod tidy                 # 第一次:產生 go.sum,之後請提交
go run ./cmd/server         # :8080
cd web && pnpm install && pnpm dev   # :5173,已代理 /api 與 /auth
```

本機測試登入時,`OAUTH_REDIRECT_URL=http://localhost:8080/auth/callback`、`APP_BASE_URL=http://localhost:8080`,並在 Google Cloud 加上同一個 redirect URI。

## 測試

```bash
go test ./...
# 含資料庫整合測試(請用拋棄式資料庫):
TEST_DATABASE_URL=postgresql://... go test ./internal/auth/
```

## 部署到 Railway

1. Railway 新增服務(連結此 repo)與一個 Postgres。服務會使用 `Dockerfile` 與 `railway.json`(單實例、`/healthz` 健康檢查)。
2. Google Cloud Console → API 與服務 → 憑證 → 建立「OAuth 用戶端 ID」(網頁應用程式),重新導向 URI 填 `https://<服務網域>/auth/callback`。
3. 在 Railway 服務填入 `.env.example` 內所有變數。`DATABASE_URL` 用參照 Postgres 服務的變數。
4. 部署後開 `https://<服務網域>/healthz` 應回 `ok`;用 `ALLOWED_EMAIL` 的帳號登入。
5. iPhone:Safari 開網址 → 分享 → 加入主畫面。

## 驗收清單(階段 1)

- 白名單帳號可登入,重新整理後仍保持登入。
- 其他 Google 帳號登入後看到「沒有使用權限」,資料庫 `users` 沒有新增資料。
- 未登入呼叫 `GET /api/me` 回 401。
- 登出後 cookie 被清除,`/api/me` 回 401。
- 加到 iPhone 主畫面後可開啟,且保持登入。

## 階段 2 設定

1. Google Cloud → API 與服務 → 程式庫 → 啟用 **Google Calendar API**。
2. Google Cloud → 目標對象 → **發布應用程式**(正式版)。測試狀態下 refresh token 只有 7 天壽命。未經驗證的正式版登入時仍會出現警告,點「進階 → 前往」即可。
3. Railway 新增變數:`ENCRYPTION_KEY`、`GEMINI_API_KEY`、`LLM_MODEL`(見 `.env.example`)。**缺任何一個服務都會啟動失敗。**
4. 部署後登入,首頁點「連結 Google 行事曆」,授權時要勾選行事曆權限。系統會自動建立名為「計畫助理」的行事曆。
5. 若之前在測試狀態下授權過,發布為正式版後請重新按一次「連結 Google 行事曆」取得新的 refresh token。

## 階段 2 驗收清單

- 輸入「下週三下午兩點看牙醫」→ 確認畫面日期正確、時長 90 分鐘並標示「預設值」。
- 輸入「晚點去繳費」→ 開始時間空白,並出現需勾選的模糊提示;未補齊前無法送出。
- 按確認後,Google 行事曆的「計畫助理」行事曆出現該行程;連按兩次確認只會有一筆。
- 在 App 內刪除行程,Google 上的對應行程一併消失。
- 麥克風:iPhone 主畫面 App 內按麥克風、允許權限、說一句話,文字出現在輸入框。
- 到 Google 帳戶「第三方存取」移除本 App 的授權後再寫入,畫面顯示「需重新授權 Google」。

## 測試

```bash
go test ./...   # 涵蓋時間正規化、預設時長、Gemini 請求格式、行事曆寫入與 invalid_grant、確認冪等
```

## 階段 3 設定(LINE 提醒)

1. LINE Developers Console → 你的 Messaging API 頻道:
   - **Basic settings → Channel secret** → Railway 變數 `LINE_CHANNEL_SECRET`
   - **Messaging API → Channel access token**(長期有效,按 Issue)→ `LINE_CHANNEL_ACCESS_TOKEN`
2. **先部署,再填 Webhook。** 部署完成後,Messaging API 分頁 → Webhook URL 填
   `https://<你的網域>/line/webhook` → 開啟 **Use webhook** → 按 **Verify**,應顯示 Success。
3. 到 LINE Official Account Manager 的回應設定,**關閉自動回應訊息**(避免多耗每月 200 則額度)。
4. 服務必須維持**單一實例**(`railway.json` 已設定 `numReplicas: 1`)。排程器在程式內每分鐘掃描。

## 階段 3 驗收清單

- 首頁「綁定我的 LINE」→ 出現 8 碼 → 在 LINE 官方帳號傳送該碼 → 回覆「綁定成功」,首頁提示自動消失。
- 新增行程(例如 2 小時後),確認畫面「通知誰」預設**沒有勾選**;勾選自己、選「1 小時前」→ 確認。
- 到時間 LINE 收到提醒(標題、時間、地點、行前注意事項),**只收到一則**。
- 建立「30 分鐘後開始」的行程並選 1 小時前提醒 → 立刻收到,訊息末尾註明提醒時間已過。
- 在 App 內刪除尚未提醒的行程 → 不會再收到提醒。
- 封鎖官方帳號 → 首頁顯示封鎖提示;解除封鎖(follow)後恢復。
- 用錯誤的綁定碼連傳 6 次 → 第 6 次起回覆「嘗試次數過多」。
- 用 `curl` 直接 POST `/line/webhook` 且不帶簽章 → 回 401。

## 資料表(階段 3 新增)

`contacts`(聯絡人)、`binding_codes`(只存雜湊)、`reminder_rules`、`deliveries`(`UNIQUE(event_id, contact_id, lead_minutes)` 防重複)、`line_usage`(每月已推送則數)。

## 階段 4 設定

可選的 Railway 變數:

| 變數 | 說明 |
|---|---|
| `LINE_MONTHLY_LIMIT` | 每月額度,預設 200;`0` 代表不限制 |
| `LINE_ADD_FRIEND_URL` | 邀請文字裡的加好友連結:`https://line.me/R/ti/p/@191xcckr` |

沒設 `LINE_ADD_FRIEND_URL` 時,邀請文字會改成「把我的 LINE 官方帳號加為好友」。

## 階段 4 驗收清單

- 首頁左上「聯絡人」→ 新增「媽媽」→ 「產生邀請」→ 複製(或分享)那段文字傳給對方。
- 對方加好友、傳送綁定碼 → 對方收到「綁定成功」,你的畫面自動變成「已啟用」。
- 建立分組「家人」並勾選成員;新增行程時確認畫面出現「家人」按鈕,點一下就勾選全部成員。
- 確認畫面顯示「這次提醒會用 N 則 LINE 額度(本月剩 M 則)」。
- 同一行程通知你與媽媽:你收到**含行前注意事項**的完整版,媽媽收到只有標題、時間、地點、備註的精簡版。
- 通知兩位以上的家人(內容相同)→ 只送出一次 multicast;「本月已發送」增加的是人數。
- 「暫停」媽媽後新增行程,她不在可勾選名單;已排定的提醒會被跳過。
- 媽媽封鎖官方帳號 → 她的狀態變「已失效」;解除封鎖並重新邀請後恢復。
- 將 `LINE_MONTHLY_LIMIT` 暫時設成 `1` 測試:第二則起不再發送,首頁顯示額度已用完。測完改回 200。
- 刪除媽媽 → 她從清單與分組中消失,她的待發提醒一併取消。

## 資料表(階段 4 新增)

`contacts.enabled`、`contact_groups`、`contact_group_members`。
