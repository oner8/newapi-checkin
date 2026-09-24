GO      ?= go
BINARY  ?= bin/newapi-checkin

.PHONY: build test vet fmt check run-once probe docker-build docker-up docker-logs clean

# 本地编译到 bin/newapi-checkin
build:
	$(GO) build -o $(BINARY) .

# 运行全部单元测试
test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

# 与 GitHub Actions 的 CI 等价的一键校验（gofmt + vet + test -race）
check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "以下文件未格式化（运行 make fmt 修复）："; \
		echo "$$unformatted"; \
		exit 1; \
	fi; \
	echo "gofmt OK"
	$(GO) vet ./...
	$(GO) test -race ./...

fmt:
	$(GO) fmt ./...

# 只跑一次就退出（适合 cron / GitHub Actions）
# 前置条件：config.yaml 里 database.path 可写（本地建议 ./data/newapi-checkin.db，容器内是 /data/newapi-checkin.db）
run-once:
	mkdir -p data
	$(GO) run . --run-once

# 凭据与站点诊断（只读 config.yaml，不打开数据库）
# 注意：本地运行时 config.yaml 里 database.path 需指向可写路径（如 ./data/newapi-checkin.db），
# 否则 run-once / 常驻模式会因无法创建 /data 而失败
probe:
	$(GO) run . --probe

docker-build:
	docker compose build

docker-up:
	docker compose up -d

docker-logs:
	docker compose logs -f

clean:
	rm -rf bin
