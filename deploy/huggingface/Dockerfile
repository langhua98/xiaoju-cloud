# Hugging Face Space（Docker SDK）镜像。
# 由 .github/workflows/deploy-hf.yml 把整个仓库源码上传到 Space，并把本文件放到 Space 根目录作为 Dockerfile。
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/xiaoju ./cmd/xiaoju

FROM alpine:3.20
# vips（含 HEIC 支持）给照片做缩略图，ffmpeg 给视频截图（见 internal/thumb）
RUN apk add --no-cache ca-certificates tzdata vips-tools vips-heif ffmpeg \
    && adduser -D -u 1000 user
COPY --from=build /out/xiaoju /usr/local/bin/xiaoju
# Spaces 以 uid 1000 运行，只有家目录和 /tmp 可写，且重启后数据会丢失（数据库和 Telegram 才是持久存储）
USER user
WORKDIR /home/user/app
ENV XIAOJU_HTTP_ADDR=:7860 \
    XIAOJU_STORAGE=telegram \
    XIAOJU_TMP_DIR=/tmp \
    TG_SESSION_FILE=/home/user/app/data/tg-session.json
EXPOSE 7860
ENTRYPOINT ["xiaoju"]
CMD ["serve"]
