# ============================================
# NVIDIA 驱动检索 — Docker 镜像
# 多阶段构建，最终镜像 ~15MB
# ============================================

# ── 编译阶段 ──
FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY go.mod ./
COPY *.go ./
COPY cmd/ ./cmd/
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o server .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o updater ./cmd/updater/

# ── 运行阶段 ──
FROM alpine:3.21

# 时区与 CA 证书
RUN apk add --no-cache ca-certificates tzdata && \
    cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && \
    echo "Asia/Shanghai" > /etc/timezone

# 非 root 运行。先建用户，static/ 才能带着正确属主拷入，
# 否则 updater 以 appuser 身份写 /static/data 会因权限被拒。
# UID 固定为 1000：与 docker-compose.yml 中 user: "1000:1000" 保持一致，
# 以便容器进程能写入宿主机 bind mount 的 static/data 目录。
RUN adduser -D -g '' -u 1000 appuser

COPY --from=builder /app/server /server
COPY --from=builder /app/updater /updater
COPY --chown=appuser:appuser static/ /static/

EXPOSE 8090

USER appuser

CMD ["/server"]
