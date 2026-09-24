# 多阶段构建：编译期用 golang:1.25-alpine，运行期用精简的 alpine:3.20。
# SQLite 驱动为纯 Go 实现，因此可以 CGO_ENABLED=0 静态编译。

# ---------- 构建阶段 ----------
FROM golang:1.25-alpine AS build

# 版本号写入二进制（可用 --version 查看）；docker compose build --build-arg VERSION=1.2.3
ARG VERSION=dev

WORKDIR /src

# 先拷贝依赖清单，利用层缓存。
COPY go.mod go.sum ./
RUN go mod download

# 再拷贝源码并编译为静态二进制。
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/newapi-checkin .

# ---------- 运行阶段 ----------
FROM alpine:3.20

# 运行期必须的两个系统包（alpine 基础镜像两者都不含，缺了会导致容器跑不起来）：
#   ca-certificates —— Go 使用系统证书池，缺失时所有 HTTPS 请求报
#                      "x509: certificate signed by unknown authority"（签到与 Bark 全部失败）
#   tzdata          —— 提供 /usr/share/zoneinfo，缺失时 time.LoadLocation("Asia/Shanghai")
#                      报 "unknown time zone"，配置校验直接失败、容器无法启动
# 若不希望带 tzdata（约 3MB），可改为在 Go 代码里 import _ "time/tzdata" 把时区库编进二进制。
RUN apk add --no-cache ca-certificates tzdata

# 非 root 用户（uid 10001）。
RUN adduser -D -u 10001 -h /app newapi-checkin

WORKDIR /app

COPY --from=build /out/newapi-checkin /app/newapi-checkin

# /data 作为数据卷存放 SQLite 数据库。
RUN mkdir -p /data && chown -R 10001:10001 /data
VOLUME ["/data"]

USER 10001

EXPOSE 8080

# 使用 busybox 自带的 wget 做存活探测。
# 注意：若在 config.yaml 里把 server.enabled 设为 false（或启动时加了 --no-server），
# 健康检查会失败、容器被标记为 unhealthy，此时请一并删除本节 healthcheck。
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q --spider http://127.0.0.1:8080/healthz || exit 1

# 无参数即常驻调度；如需一次性执行可用 docker run ... --run-once。
ENTRYPOINT ["/app/newapi-checkin"]
