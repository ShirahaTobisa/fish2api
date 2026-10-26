# 服务器版:API + 面板 + 无头 Chromium。
# 登录仍在本地电脑完成,token 推送/粘贴过来;容器里的 chromium 专门用来收割
# 网页合成(POST /task)每次要求的 reCAPTCHA 人机验证 token,所以服务器也能出声。
FROM golang:1.26.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /fish2api .

FROM alpine:3.21
# chromium:给 reCAPTCHA 收割用(容器以 root 运行时,程序会自动加 --no-sandbox/--disable-dev-shm-usage)
RUN apk add --no-cache ca-certificates tzdata chromium
COPY --from=build /fish2api /usr/local/bin/fish2api
ENV HOST=0.0.0.0 PORT=8765 ACCOUNTS_FILE=/data/accounts.json DISABLE_LOGIN=1
WORKDIR /data
EXPOSE 8765
HEALTHCHECK --interval=30s --timeout=5s CMD wget -qO- http://127.0.0.1:8765/health >/dev/null || exit 1
CMD ["fish2api"]
