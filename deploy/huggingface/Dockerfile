# Hugging Face Space（Docker SDK）专用镜像：构建时从 GitHub 私有仓库拉取源码。
# 需要在 Space 的 Settings → Variables and secrets 中添加 Secret：GITHUB_TOKEN
# （GitHub fine-grained token，只授予 langhua98/xiaoju-cloud 的 Contents: Read-only 权限）。
FROM golang:1.25-alpine AS build
RUN apk add --no-cache git
ARG XIAOJU_REPO=github.com/langhua98/xiaoju-cloud.git
ARG XIAOJU_REF=main
# 修改这个值（或在 Space 设置里 Factory rebuild）可以强制重新拉取最新代码
ARG CACHE_BUST=1
RUN --mount=type=secret,id=GITHUB_TOKEN,mode=0444,required=true \
    git clone --depth 1 --branch "${XIAOJU_REF}" \
      "https://x-access-token:$(cat /run/secrets/GITHUB_TOKEN)@${XIAOJU_REPO}" /src \
    && git -C /src log -1 --oneline
WORKDIR /src
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/xiaoju ./cmd/xiaoju

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
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
