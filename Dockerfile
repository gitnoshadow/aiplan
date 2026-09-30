# --- 1) frontend ---
FROM node:22-alpine AS web
WORKDIR /web
RUN npm install -g pnpm
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm build

# --- 2) backend ---
FROM golang:1.23-alpine AS server
WORKDIR /src
COPY . .
# go.sum is generated on first build; commit it after running `go mod tidy` locally.
RUN go mod tidy && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# --- 3) runtime ---
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 app
WORKDIR /app
COPY --from=server /out/server /app/server
COPY --from=web /web/dist /app/web/dist
ENV STATIC_DIR=/app/web/dist TZ=Asia/Taipei
USER app
EXPOSE 8080
CMD ["/app/server"]
