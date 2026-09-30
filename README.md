# 語音計畫助理

Go + Vue 3 (Vant) 單一服務。階段 1:骨架與 Google 登入(單一帳號白名單)。階段 2:文字或語音輸入 → Gemini 解析 → 確認畫面 → 寫入專用 Google 行事曆。

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
