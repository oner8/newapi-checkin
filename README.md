# newapi-checkin

用 Go 编写的常驻服务：每天定时对多个 [new-api](https://github.com/Calcium-Ion/new-api)（one-api 的流行分支，OpenAI 兼容 API 中转面板）站点执行「每日签到」，结果写入 SQLite，并通过 **Bark（iOS 推送）** 通知。凭据（Cookie / 访问令牌）从环境变量注入，不落明文。

> **免责声明 / Disclaimer**
>
> 本项目是**个人自用的自动化工具**，仅供学习与自用。使用前请确认你有权访问目标站点，并遵守其服务条款；
> **请勿用于批量刷号、代签、转售或其他滥用行为**。项目中的部分能力（例如自动处理前置防护的 JS 挑战、
> 为指定站点配置代理）请只在你有权访问的站点上使用。因使用本项目产生的一切后果（包括账号被封禁）
> 由使用者自行承担。

## 特性

- **多站点**：一次配置多个 new-api 站点，独立调度、并发执行。
- **双凭据**：支持 `cookie`（`session`，兼容绝大多数版本与 fork）与 `access_token`（面板个人访问令牌 PAT，走 `Authorization: Bearer`）。
- **幂等重试**：服务端 `(user_id, checkin_date)` 唯一索引，重复签到返回「今日已签到」而非报错；网络错误 / 5xx / 429 / 非 JSON 响应 / 临时性服务端提示按指数退避自动重试（见「重试规则」）。
- **SQLite 落库**：签到明细、批次日志、通知日志持久化。
- **Bark 汇总**：按站点结果汇总推送，有失败时自动升级为 `timeSensitive`。
- **启动补跑**：容器启动时若当天尚未跑过则补跑（`run_on_start`）。
- **`--probe` 诊断**：自动探测站点凭据组合、版本、签到开关与验证码开关。
- **抗中断批次**：定时批次与启动补跑使用与进程信号解耦的批处理上下文（上限 30 分钟），重启不会把批次打断成假失败，也不会污染当天的记录。

## 目标站接口契约

下面是从 new-api 源码核实过的接口行为，了解它有助于排查问题。

| 用途 | 请求 | 说明 |
| --- | --- | --- |
| 签到 | `POST /api/user/checkin` | **无请求体**。成功返回 `{"success":true,"message":"签到成功","data":{"quota_awarded":N,"checkin_date":"YYYY-MM-DD"}}` |
| 查状态 | `GET /api/user/checkin?month=YYYY-MM` | 读 `data.stats.checked_in_today` / `data.stats.total_quota` |
| 免登录探测 | `GET /api/status` | 公开返回 `checkin_enabled`、`turnstile_check`、`quota_per_unit`、`system_name`、`version` |

- **鉴权**：两个签到接口都在 `middleware.UserAuth()` 下。
  - 新版（v1.0.0-rc.x）用 `Authorization: Bearer <token>`，token 可以是**面板会话 token**或**个人访问令牌（PAT）**，PAT 在面板 `/api/user/token` 生成。
  - 老版本 / 各类 fork 常用 `session` Cookie。
  - 部分老版本使用访问令牌时还需要额外请求头 `New-Api-User: <用户ID>`。
- **Turnstile**：`POST /api/user/checkin` 上挂着 `middleware.TurnstileCheck()`，开启时验证码 token 通过**查询参数** `?turnstile=xxx` 传递，因此纯脚本**无法**自动通过 Cloudflare Turnstile。这正是必须使用长期有效 Cookie / 令牌的原因。
- **幂等**：服务端对 `(user_id, checkin_date)` 建了唯一索引，同日重复签到返回「今日已签到」，重试安全。
- **额度单位**：`quota_awarded` 是内部单位，显示金额 = 额度 / `quota_per_unit`（默认 `500000` 表示 $1）。
- **签到开关可能缺失**：`/api/status` 里没有 `checkin_enabled` 字段时（fork 常见），本工具按「未声明」处理并**继续尝试签到**，不会像以前那样直接判站点失败——日志里会有一条 `站点 /api/status 未声明签到开关，按开启处理并继续尝试`。真正不支持的旧版站点会在签到请求上返回 404。
- **`New-Api-User` 可能是硬要求**：部分 fork 用 Cookie 鉴权时强制要求该头，缺了会返回 `401 {"message":"…未提供 New-Api-User"}`。把用户 ID 填进 `credential.new_api_user` 即可（`--probe` 会分别试 `cookie` 与 `cookie+New-Api-User`，直接告诉你哪种可用）。
- **路径可覆盖**：上表是 new-api 标准路径，但**部分 fork 改过签到接口**（例如改成 `POST /api/user/sign_in`，响应形如 `{"success":true,"message":"签到成功，获得 $25 额度"}`，连 `data` 都没有），并且**没有签到状态查询接口**。这两种差异都能在配置里声明，见「fork 改了签到路径 / 没有状态查询接口」。

## fork 改了签到路径 / 没有状态查询接口

有些第三方 fork 只保留 new-api 的鉴权方式（`session` Cookie + `new-api-user` 头），却把签到接口挪到了别的路径，而且不提供状态查询。这类站点用两个字段适配：

```yaml
  - name: 某fork站
    base_url: https://example-fork.com
    enabled: true
    checkin_path: /api/user/sign_in   # 默认 /api/user/checkin；查状态与提交都用它
    checkin_status: off               # 默认 auto；该站没有状态查询接口，写 off 省掉一次注定 404 的请求
    credential:
      type: cookie
      cookie: ${SITE_FORK_COOKIE}     # 整段 Cookie（见「被 WAF / JS 挑战拦截」一节）
      new_api_user: "12345"
    headers:
      User-Agent: "Mozilla/5.0 …"     # 与取 Cookie 的浏览器一致（若站点前面有 WAF）
```

行为说明：

- `checkin_path` 同时用于查状态（`GET`）与提交（`POST`）；省略则用 `/api/user/checkin`。只写路径，不要写完整 URL。
- `checkin_status: off` **完全不发**状态查询请求，直接提交。适合明确没有该接口的站点（如只提供 `POST /api/user/sign_in` 的 fork）。
- `checkin_status: auto`（默认）时，如果站点对状态查询回 404/405 或结构不符，**只记一条警告并继续提交**，不会因此判站点失败——"查不到状态"不等于"签不上"。日志里会出现 `站点没有可用的签到状态查询接口，跳过预查直接提交`，此时建议干脆写上 `checkin_status: off`。
- 这类 fork 的签到响应常常**没有 `quota_awarded` / `checkin_date`**：本工具会在签到前后各读一次 `/api/user/self` 余额，用**余额差**补出本次获得额度，并沿用配置时区的当天日期入库。
- 网络错误、5xx、凭据失效仍然照常上报（不会因为上面的"尽力而为"而掩盖真实故障）。

## 快速开始

```bash
# 1. 复制并编辑配置
cp config.example.yaml config.yaml
#    在 sites 下填入你的站点 base_url 与凭据引用

# 2. 复制并填写环境变量（凭据）
cp .env.example .env
#    填入 PANEL_TOKEN / BARK_KEY / SITE_XXX_COOKIE

# 3. 创建数据目录：容器内以非 root 用户（uid 10001）运行，而 bind mount 的 ./data
#    由 dockerd 创建时属主是 root，容器会无法创建 /data/newapi-checkin.db，并在 restart 策略下反复重启。
sudo install -d -o 10001 -g 10001 ./data

# 4. 启动（常驻调度）
docker compose up -d

# 5. 查看日志
docker compose logs -f
```

访问 `http://127.0.0.1:8080/healthz` 应返回 200；`docker compose ps` 的 STATUS 应最终变为 `healthy`。

> **容器反复重启，日志报 `打开数据库 /data/newapi-checkin.db 失败: ... permission denied`**：`./data` 属主不对，执行 `sudo chown -R 10001:10001 ./data && docker compose restart`。
> **日志报读取 `/app/config.yaml` 失败**：配置文件是只读挂载进容器的，权限需为 `644`（`sudo chmod 644 config.yaml`）；同理，用 `SITE_XXX_COOKIE_FILE` 挂 secret 文件时该文件也要对 uid 10001 可读。

> **不走 Docker 的本地运行**：示例配置里的 `database.path` 是容器内路径 `/data/newapi-checkin.db`，本地直接 `go run . --run-once` / `make run-once` 会因无法创建 `/data` 而失败；请改成 `./data/newapi-checkin.db`（或任何可写目录）。

## 如何从浏览器取 Cookie

Cookie 适用于**较老版本的 new-api 与大部分 fork**（要求站点仍使用 `session` Cookie 会话）。

> **新版 new-api（`v1.0.0-rc.x` 起 / 上游 main）不适用**：它已改为「15 分钟 JWT Access Token + HttpOnly Refresh Cookie」，浏览器里没有可复制的 `session` Cookie。请直接看下一节用 PAT。判断方法：先跑 `--probe`，若打印的版本是 `v1.0.0-rc.` 开头且凭据探测全部 401 `AUTH_UNAUTHORIZED`，就是这种站点。

1. 浏览器登录你的 new-api 面板。
2. 按 `F12` 打开开发者工具。
3. 切到 **Application（应用程序 / 存储）** 面板 → 左侧 **Cookies** → 选中面板域名。
4. 找到名为 `session` 的 Cookie，**复制它的值**（一整串字符）。
5. 更稳妥的做法：切到 **Network（网络）** 面板，刷新页面，点开任意一个请求，在 **Request Headers（请求头）** 里找到整段 `Cookie:`，把它整段复制下来。

把复制到的内容填进环境变量，例如：

```bash
SITE_EXAMPLE_COOKIE=session=xxxxxxxx
# 也可以只填裸值，脚本会自动补上 session= 前缀：
SITE_EXAMPLE_COOKIE=xxxxxxxx
```

然后在 `config.yaml` 的站点里引用：

```yaml
credential:
  type: cookie
  cookie: ${SITE_EXAMPLE_COOKIE}
```

> **Cookie 会过期。** 失效后签到会返回 `auth_failed`，届时会收到 Bark 的「凭据失效」提醒，需要重新按上述步骤复制并更新 `SITE_EXAMPLE_COOKIE`。

## access_token 模式

**新版 new-api（`v1.0.0-rc.x` 起 / 上游 main）只能用这种方式。** 该版本把面板鉴权换成了「15 分钟有效期的 JWT Access Token（只存在浏览器内存）+ HttpOnly Refresh Cookie（`Path=/api/user/auth`）」，其官方文档原文：「面板请求不再依赖 Gin session，也不再要求 `New-Api-User` 请求头」，同时保留 PAT 契约 `Authorization: Bearer <pat>`。因此：

- 浏览器里**不再有可用的 `session` Cookie**；`new_api_refresh` 是 HttpOnly 且带路径限制的刷新令牌，脚本拿它没用。实测四种组合——作为 Cookie（名为 `session`）、作为 Cookie（名为 `new_api_refresh`）、作为 `Authorization: Bearer`、再补 `New-Api-User` 头——全部返回 401 `AUTH_UNAUTHORIZED`。
- 内存里的 Access Token 只有 15 分钟有效期且不落盘，无法作为长期凭据。
- 结论：**这类站点必须用 PAT**。

获取步骤（面板：个人设置 → 账户管理 → 安全设置 → 生成令牌）：

1. 登录面板 → **个人设置 → 账户管理 → 安全设置** → 点「生成令牌」，复制生成的 `sk-...` 值。
2. 填入环境变量并在配置中引用：

```yaml
credential:
  type: access_token
  token: ${SITE_EXAMPLE_TOKEN}
```

3. 若目标站是**老版本 / fork**，请求可能提示缺少用户标识头，此时在凭据里补上 `new_api_user`：

```yaml
credential:
  type: access_token
  token: ${SITE_EXAMPLE_TOKEN}
  new_api_user: "123"   # 你的用户 ID（F12 → Local Storage → user.id）
```

该值会作为 `New-Api-User` 请求头发送；新版 new-api 已忽略它（无害）。

## 同一站点多个账号（多用户）

同一个网站有多个账号时，**在 `sites` 下写多条、`base_url` 重复、`name` 各不相同**即可，不需要任何额外开关或额外进程。

```yaml
sites:
  - name: 示例站-A
    base_url: https://api.example.com
    enabled: true
    credential:
      type: access_token
      token: ${SITE_EXAMPLE_A_TOKEN}

  - name: 示例站-B
    base_url: https://api.example.com     # 同一个站点，重复写
    enabled: true
    credential:
      type: access_token
      token: ${SITE_EXAMPLE_B_TOKEN}
```

```bash
# .env：每个账号一份凭据（每个账号在面板里各自生成 PAT，或各自取 Cookie）
SITE_EXAMPLE_A_TOKEN=sk-...
SITE_EXAMPLE_B_TOKEN=sk-...
```

为什么各账号互不干扰：

- **站点身份用 `name`，不是 `base_url`**。签到记录的唯一索引是 `(site_name, checkin_date)`，所以同一天每个账号各占一行。`name` 重复会被配置校验直接拒绝（报 `name 与 sites[i] 重复`），而 `base_url` 重复是允许的。
- `/api/user/self`（余额、用户 ID）与 `GET /api/user/checkin?month=`（今天是否已签）都是**按凭据**返回的，A 已签到不会让 B 被跳过。
- 抖动按 `name + 日期` 哈希，两个账号不会在同一秒打过去。
- 通知是一条汇总（所有站点与账号列在同一条消息里）。失败去重键是 `fail|日期|站点名|状态`、全绿汇总键是 `ok|日期`，所以两个账号都成功也只推一条「签到完成」。

注意事项：

- **`name` 是数据库与日志里的身份**：以后改名等于换了一个「新站点」，旧名字当天的记录不会迁移（历史仍按 `site_name` 留在库里）。一开始就用能长期区分的名字，例如 `示例站-A` / `示例站-B`。
- 每个账号要**各自的凭据**，且凭据类型可以每个账号单独选（例如 A 用 PAT、B 用 Cookie，取决于站点支持哪个）。
- 同站多账号共用你的出口 IP，站点可能有限流：`app.concurrency` 是全局的（默认 3），多账号时不要为求快调大；错开时间交给 `app.jitter_seconds`（默认 600 秒）。
- 只针对某个账号诊断 / 执行：`--probe --site 示例站-B`、`--run-once --site 示例站-B`（按 `name` 精确匹配）。
- 改了 `.env` 需要 `docker compose up -d` 重建容器；只改 `config.yaml`（挂载文件）则 `restart` 即可生效。


## 站点需要代理

有的站点直连不通（被墙、或只在特定网络可达），两种做法：

**1）全局：环境变量**（最省事）——写进 `.env`，容器通过 compose 的 `env_file` 读到：

```bash
HTTPS_PROXY=http://host.docker.internal:7890
HTTP_PROXY=http://host.docker.internal:7890
# 不需要走代理的站点用 NO_PROXY 排除（逗号分隔，支持域名后缀）
NO_PROXY=api.example.com,another.example.net
```

**2）按站点：`sites[].proxy`**（推荐，出口可控、互不影响）：

```yaml
  - name: 需要梯子的站
    base_url: https://blocked.example.com
    enabled: true
    proxy: ${SITE_BLOCKED_PROXY}   # 建议从环境变量注入，代理凭据不进配置文件
    credential:
      type: access_token
      token: ${SITE_BLOCKED_TOKEN}
```

```bash
# .env：支持 http:// 、https:// 、socks5://（可带用户名密码）
SITE_BLOCKED_PROXY=socks5://user:pass@127.0.0.1:1080
```

- 留空 = 沿用环境变量；两者都配时**站点自己的 `proxy` 优先**。
- `socks5://` 时域名由代理解析（适合本机 Clash/mihomo 的 socks 端口）。
- `--probe --site X` 会回显该站生效的代理（凭据打码），运行日志里也有 `proxy=` 字段。
- 代理写错（缺协议 / 协议不支持）会在**配置校验**阶段直接报错，不会静默直连。

### 容器里怎么填宿主机的代理

容器里的 `127.0.0.1` 是**容器自己**，不是宿主机。要连宿主机上的代理：

- 本仓库的 `docker-compose.yml` 已加 `extra_hosts: host.docker.internal:host-gateway`，直接写
  `http://host.docker.internal:7890` 即可（Linux 上同样生效）；
- 或用宿主机局域网 IP（如 `http://192.168.1.10:7890`）；
- 或用 Docker 网关地址（默认 bridge 为 `172.17.0.1`，compose 自建网络通常是 `172.18.0.1`，可用 `docker network inspect` 确认）。

> **容器连不上宿主机代理的两个常见原因**：
> 1. **代理只监听 `127.0.0.1`**（Clash/mihomo 默认 `allow-lan: false`）——那容器无论用 `host.docker.internal` 还是宿主机 IP 都连不上。要么打开 `allow-lan` / 让它监听 `0.0.0.0`，要么把代理也跑成容器并加入同一个 docker 网络（用服务名访问）。
> 2. **宿主机防火墙拦住了 docker 网段到宿主端口的访问**（本机实测：ufw 默认策略下，容器访问 `172.18.0.1:<端口>` 会超时）。放行即可，例如 `sudo ufw allow from 172.17.0.0/16 to any port 7890 proto tcp`（网段用 `docker network inspect` 确认）。
>
> 自测一条命令：在容器里执行 `docker compose exec newapi-checkin wget -qO- http://host.docker.internal:7890` —— 能拿到代理的响应（哪怕是错误页）就是通的，超时则是上面两种情况之一。

### 注意：代理会改变出口 IP

阿里云 WAF 的 `acw_sc__v2`、Cloudflare 的 `cf_clearance` 这类通行 Cookie **与出口 IP + User-Agent 绑定**。
若某站要走代理，那么**取通行 Cookie 时也要从同一个出口**（浏览器同样走那条代理），否则会一直被挑战；
换节点/出口变化后需要重新取一次。

## 使用 `--probe` 诊断

`--probe` 只尝试「手头确实有材料的凭据组合」，没有材料的组合直接跳过：

- 配了 `cookie`：试 `cookie`；若同时配了 `new_api_user`，再试 `cookie+New-Api-User`。
- 配了 `token`：试 `access_token` 与 `access_token+New-Api-User`（用户 ID 未知时用 `1` 探测）。

同时打印目标站的 `version`、签到开关（`checkin_enabled`）与 Turnstile 开关（`turnstile_check`）。

```bash
# 本地
go run . --probe
# 或
make probe

# 只诊断某个站点
go run . --probe --site 某某中转

# 容器内
docker compose run --rm newapi-checkin --probe
```

输出示例（一个只配了 cookie、没配 token 的站点。配了 token 的站点还会多出 `access_token` 与 `access_token+New-Api-User` 两行）：

```
站点: 某某中转
  base_url: https://api.example.com
  new-api 版本: v1.0.0-rc.3（站点名: 某某中转）
  签到功能: 已开启   Turnstile 验证码: 未开启
  凭据探测:
    ✓ cookie                       用户=example(id=123) 余额=500000
  建议: credential.type=cookie
```

## 命令行参数

| 参数 | 说明 |
| --- | --- |
| `--config <路径>` | 配置文件路径，默认 `config.yaml` |
| `--run-once` | 跑一次就退出，适合 cron / GitHub Actions |
| `--probe` | 凭据与站点诊断 |
| `--site <名称>` | 只处理指定站点 |
| `--dry-run` | 只探测不提交签到 |
| `--no-server` | 不启动 HTTP 服务 |
| `--log-level <debug\|info\|warn\|error>` | 日志级别 |
| `--version` | 打印版本 |

## 环境变量规则

- `${VAR}`：变量未定义时**启动即失败**，并报出是哪个字段。
- `${VAR:-}`：允许为空。
- `${VAR:-默认值}`：支持默认值。
- 若同时设置了 `VAR_FILE`（例如 `SITE_EXAMPLE_COOKIE_FILE=/run/secrets/cookie`），则从该文件读取内容（去除首尾空白），方便配合 Docker secret。
- 站点凭据 env 命名建议 `SITE_<站点名大写蛇形>_COOKIE`。
- Cookie 值**原样透传，不做 URL 解码**（`session` 值里常见的 `%3D%3D` 必须保持原样）；也接受只填写裸值（脚本会自动补 `session=` 前缀）。
- 例外提醒：值里出现 `${` 且后面看起来像变量名时，会被当作变量引用（漏写 `}` 会在启动时直接报错并指出字段）。这是刻意的 fail-fast，避免漏写花括号后被静默当成凭据、到运行时才表现为"凭据失效"。
- 凭据不会明文写入数据库；容器日志与 `--probe` 输出里只显示掩码（形如 `cookie[session](abcd…wxyz)`）。

## HTTP 接口

| 方法 | 路径 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/healthz` | 无 | 200，Docker healthcheck 用 |
| `GET` | `/api/sites` | 无 | 每站配置概要与最近一次签到状态；`credential` 是元信息对象，不含任何凭据片段 |
| `GET` | `/api/runs?limit=20` | 无 | 历史批次 |
| `GET` | `/` | 无 | 接口索引 |
| `POST` | `/api/run` | `X-Admin-Token` | 异步触发一次，返回 `run_id`；可用 `?site=<名称>` 只跑一个 |

**鉴权边界**：只有 `POST /api/run` 受 `server.admin_token` 保护（请求头 `X-Admin-Token: <admin_token>`，即环境变量里的 `PANEL_TOKEN`）；未配置 `server.admin_token` 时只接受来自 `127.0.0.1` 的请求。`GET /healthz`、`GET /api/sites`、`GET /api/runs`、`GET /` 都是**未鉴权的只读接口**：它们不返回任何密钥、Cookie 或令牌片段，但仍会暴露站点名与 `base_url`。

> **部署建议**：这些读接口没有鉴权，若把端口发布到公网，请改成只绑定本机（compose 里写 `127.0.0.1:8080:8080`）或不发布端口。

### `/api/sites` 的 `credential` 字段

每个站点的 `credential` 是一个对象（不是字符串），并且**不会返回任何凭据片段 —— 连首尾字符也不返回**：

```json
"credential": {
  "type": "cookie",
  "cookie_names": ["session"],
  "has_secret": true,
  "new_api_user": false
}
```

| 字段 | 含义 |
| --- | --- |
| `type` | `cookie` 或 `access_token` |
| `cookie_names` | 仅在 `type=cookie` 时出现：Cookie 里的字段名（如 `session`），不含取值 |
| `has_secret` | 是否配置了凭据内容（cookie 或 token） |
| `new_api_user` | 是否配置了 `New-Api-User` 头 |

`cookie[session](abcd…wxyz)` 这种带首尾字符的掩码形式只出现在 `--probe` 输出与容器日志里，不会出现在该接口的响应里。

示例：

```bash
curl -H "X-Admin-Token: $PANEL_TOKEN" http://127.0.0.1:8080/api/run
curl -H "X-Admin-Token: $PANEL_TOKEN" "http://127.0.0.1:8080/api/run?site=某某中转"
curl http://127.0.0.1:8080/api/sites
curl "http://127.0.0.1:8080/api/runs?limit=20"
```

## 状态枚举

| 状态 | 含义 |
| --- | --- |
| `success` | 签到成功 |
| `already` | 今日已签到（跳过） |
| `skipped_disabled` | 站点未开启签到功能 |
| `auth_failed` | 凭据失效 |
| `need_turnstile` | 站点要求过验证码 |
| `not_newapi` | 打不开 `/api/status` 或响应不像 new-api |
| `blocked` | 请求被站点前置的 WAF/防护拦截（JS 挑战页、边缘拒绝），脚本无法通过 |
| `dry_run` | 试运行未提交 |
| `error` | 其他错误 |

## 前置防护 / JS 挑战

有些站点在 new-api 前面挂了 WAF（阿里云 ESA/WAF、Cloudflare 等），会用**返回 JS 挑战页**的方式把非浏览器请求挡在门外。

**阿里云 WAF 的挑战会被自动通过**（默认行为，无需任何配置）：它的 `acw_sc__v2` 挑战做的是"对页面里的 `arg1` 做固定位置重排，再与固定密钥异或"，是纯字符串变换，因此本工具直接算出通行 Cookie 并重放一次请求即可通过。日志里会看到：

```
WARN 站点 /api/status 未声明签到开关，按开启处理并继续尝试 site=... version=v0.0.0
INFO 已自动通过站点前置防护的 JS 挑战（acw_sc__v2） site=...
```

- 已验证：`server: ESA` + `x-tengine-error: denied by http_custom` 这类站点，未做任何人工配置即可正常签到。
- 想关掉这个行为（例如你希望显式失败）就设 `app.waf_challenge: off`，此时这类响应会按下面的 `blocked` 处理。
- 若凭据里粘贴了旧的 `acw_sc__v2`，工具在重放时会把它丢掉（同名 Cookie 会让服务端取到过期那个）。
- **其他厂商的挑战（Cloudflare 等）仍需手动提供通行 Cookie**，见下一节。

## 被 WAF / JS 挑战拦截（`blocked`）

有些站点在 new-api 前面挂了 WAF/防护（阿里云 ESA/WAF、Cloudflare 等），会用**返回 JS 挑战页**的方式把非浏览器请求挡在门外。典型特征：

- HTTP 状态可能是 **200**（也可能 403），但响应体是 HTML（`var arg1=…`、`Just a moment...`、`document.cookie` 之类）
- 响应头会暴露边缘节点：`server: ESA`、`x-tengine-error: denied by …`、`cf-mitigated: challenge`、`set-cookie: acw_tc / cdn_sec_tc`

工具把**无法自动求解**的这类响应判为 `blocked`，**不做重试**（重试多少次都不会变成 JSON），并在日志里给出成因与做法。要让它通过：

1. 在浏览器里打开该站，等它自动通过挑战（页面正常显示）。
2. `F12` → **Network** → 点任意请求 → **Request Headers** → 复制**整段** `Cookie:`（通常包含 `session` 以及 `acw_sc__v2`、`acw_tc`、`cdn_sec_tc`、`cf_clearance` 之类的通行 Cookie）。
3. 把整段粘进对应环境变量（**含分号，会原样发送**，不要只贴 `session` 的值）：

```bash
SITE_XXX_COOKIE="session=…; acw_sc__v2=…; acw_tc=…"
```

4. 关键：**把 `headers.User-Agent` 设成与取 Cookie 时同一个浏览器 UA**。通行 Cookie 通常与 IP + User-Agent 绑定，UA 不一致会立刻再次被拦：

```yaml
    headers:
      User-Agent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36"
```

5. 用 `--probe` 验证是否已通过（`✓ cookie` 即成功）。

注意：这类通行 Cookie **有有效期**（往往几小时到几天），过期后会再次 `blocked`，需要重新获取——这是站点侧防护带来的固有成本，不是工具能绕过的。若站点同时有 Turnstile，仍然无解（见「常见问题」）。

> 顺带说明：`blocked` 与 `auth_failed` 要分清楚。`auth_failed` 是**请求到了 new-api** 但凭据不被接受（应重新取 Cookie/令牌）；`blocked` 是**请求根本没到**（要处理前置防护）。

## 重试规则

`app.retries` 是单个站点的最大重试次数，重试做**指数退避**：首次等待 `app.retry_backoff_seconds`，之后每次翻倍；如果响应带 `Retry-After` 头，则以它为准。

**会重试**：

- 网络超时 / 连接失败；
- HTTP 5xx；
- HTTP 429；
- 响应无法解析成 JSON（多为 WAF / Cloudflare 的临时拦截页）；
- 响应文案命中临时性关键词（「请稍后重试 / 稍后再试 / 系统繁忙 / 超时 / timeout / busy」）—— 这类响应看起来是普通业务错误，但会被当作可重试的服务端错误。

**不重试**（直接作为最终结果）：

- 今日已签到；
- 站点未开启签到；
- 凭据失效；
- 需要过 Turnstile 验证码；
- 非 new-api 站点。

## 落库与幂等语义

- `checkin_records` 里**每个站点每天只有一行**（`(site_name, checkin_date)` 唯一）。重复运行（手动重跑、启动补跑、重试）是**更新**这一行，不会插入新行。
- 更新遵循「当天最佳结果」：
  - 状态**成功后不再改写**：只要当天签到成功过，这一行的 `status` 就固定为 `success`。之后无论是失败、`already`（今日已签到）还是 `dry_run`，都不会改写它 —— 已经签到成功这个事实不会被抹掉，也不会被改成看起来「没做事」的状态。
  - `quota_awarded`（当天获得额度）取历史最大值，不会被抹成 0。也就是说晚上 20:00 再手动触发一次，不会把你早上签到拿到的额度记录清零。
- `checkin_date` 以站点为准：签到成功时优先使用站点返回的 `data.checkin_date`（站点本地日期），站点没返回或格式不对时才回退到 `app.timezone` 的当天。这样当 `app.timezone` 与站点时区不一致时，同一次签到不会被记成两天。

## 批次中断与重启

- 定时批次与启动补跑使用与进程信号解耦的批处理上下文（上限 30 分钟），所以收到 `SIGTERM` 不会把正在进行的批次打断成假失败。
- 万一批次仍被取消（例如超过 30 分钟上限），**不会写入任何半成品记录或批次记录**，因此当天不会被误判成「已经跑过」；容器重启后 `run_on_start` 会再补跑一次。

结论：重启不会导致漏签，也不会污染当天的记录。

## 故障对照表

| 现象 | 原因 | 处理 |
| --- | --- | --- |
| 收到 `auth_failed` | Cookie / 令牌失效或过期 | 重新从浏览器取 `session` Cookie 或生成新的 PAT，更新对应环境变量 |
| 收到 `need_turnstile` | 站点在签到接口上开了 Cloudflare Turnstile（`/api/status` 的 `turnstile_check=true`） | token 由浏览器 widget 生成、**一次性**、并随 `remoteip` 送 Cloudflare 校验，脚本无法自动通过；**换 PAT/换 Cookie 都没用**（`middleware.TurnstileCheck()` 挂在路由上，与凭据类型无关）。只能在浏览器手动签到，或请站点管理员关闭该开关。详见「常见问题」 |
| 收到 `not_newapi` | `/api/status` 打不开或返回内容不像 new-api | 核对 `base_url` 是否正确（含 `https://`）、该地址是否真的是 new-api 站点 |
| 只有某个站连不上、超时或被 WAF 拦，其他站正常 | 该站需要代理才能访问 | 给该站点单独配 `sites[].proxy`（容器内填宿主机代理用 `host.docker.internal`），详见「站点需要代理」 |
| 日志出现 `已自动通过站点前置防护的 JS 挑战（acw_sc__v2）` | 站点前置 WAF 的挑战被自动求解（正常，无需处理） | 无需处理；想关闭自动求解设 `app.waf_challenge: off` |
| 日志出现 `站点 /api/status 未声明签到开关，按开启处理并继续尝试` | 该 fork 的 `/api/status` 不返回 `checkin_enabled` | 正常，继续走签到；若随后报 404，说明该站确实没有签到接口 |
| 日志出现 `站点没有可用的签到状态查询接口，跳过预查直接提交` | 该 fork 没有签到状态接口（auto 模式自动降级） | 正常降级；确认后可写 `checkin_status: off` 省掉这次请求 |
| 签到接口 404，但站点能正常登录 | fork 改了签到路径 | 在浏览器 DevTools 里看「点签到」那一刻的请求路径，填到该站点的 `checkin_path`（例如 `/api/user/sign_in`） |
| 收到 `blocked` | 请求被站点前置的 WAF/防护拦下（返回 JS 挑战页，HTTP 常为 200 或 403），请求根本没到 new-api | 见「被 WAF / JS 挑战拦截（`blocked`）」一节：整段 Cookie（含 `acw_sc__v2` / `cf_clearance` 等通行 Cookie）+ 匹配的 `User-Agent` |
| 签到时间不对 | 触发时间按 `app.timezone` 解释 | 检查 `app.timezone` 与容器 `TZ`（`docker-compose.yml` 中为 `Asia/Shanghai`）是否一致 |
| Bark 收不到 | Key 为空 / `enabled: false` / `mode` 过滤 | 检查 `BARK_KEY`、`notify.bark.enabled`、`notify.bark.mode`（`summary`=有失败立即推、全绿时每天推一条；`failure`=只在有失败时推；`always`=每次都推） |
| 容器时区不对 | 镜像默认 UTC | 确认 `environment: TZ: Asia/Shanghai` 生效 |
| Cookie 里 `%3D%3D` 被解码导致失败 | 手工做了 URL 解码 | 保持原样透传，不要解码 |
| 收到 `skipped_disabled` | 站点未开启签到功能 | 在目标站开启签到，或把该站 `enabled` 设为 `false` |

## 常见问题

**为什么不自动过验证码？**
new-api 的 `POST /api/user/checkin` 挂了 `middleware.TurnstileCheck()`，验证码 token 只能通过查询参数 `?turnstile=xxx` 传入，而该 token 由 Cloudflare Turnstile 在浏览器环境中生成，纯脚本无法绕过。因此本项目依赖长期有效的 Cookie / 令牌。

**同一个网站有多个账号怎么配？**
在 `sites` 下写多条：`base_url` 重复、`name` 各不相同、每个账号各自的凭据（每个账号在面板里各自生成 PAT，或各自取 Cookie）。站点身份是 `name` 而不是 `base_url`，签到记录按 `(site_name, checkin_date)` 唯一，因此每个账号每天各占一行、互不覆盖；`name` 重复会被配置校验直接拒绝。完整示例与注意事项见「同一站点多个账号（多用户）」。

**为什么签到时间有随机抖动？**
为避免所有站点在同一秒集中请求，每站每天会在 `app.jitter_seconds` 范围内随机推迟一段时间。抖动基于确定性哈希：同一天同一站点抖动固定，重启不会改变。

**签到提示要过 Cloudflare 验证，能过吗？**

先分清是两种完全不同的东西——`--probe` 的输出与日志里的状态码就能区分：

| | Cloudflare **Turnstile**（签到接口上的验证码） | Cloudflare **WAF / Managed Challenge**（整站前置挑战） |
| --- | --- | --- |
| 怎么识别 | `--probe` 显示 `Turnstile 验证码: 已开启`（即 `/api/status` 的 `turnstile_check=true`） | 接口直接返回 `Just a moment...` 之类 HTML；状态是 `blocked`，消息里点名 `Cloudflare（cf-mitigated: …）` |
| 状态 | `need_turnstile` | `blocked` |
| 能自动过吗 | **不能**。token 由浏览器里的 widget 生成，站点再拿它去 `challenges.cloudflare.com/turnstile/v0/siteverify` 校验（带 `remoteip`）——**一次性、短有效期、与 IP 相关**；而且 `middleware.TurnstileCheck()` 挂在**路由**上，换 PAT / 换 Cookie 都没用。要过只能上真实浏览器（或第三方打码服务），本项目不做 | **能**。通过一次挑战后浏览器持有 `cf_clearance`，它是可复用的通行 Cookie（绑定 IP + User-Agent，有有效期）：把**整段** Cookie 贴进 `credential.cookie`，并把 `headers.User-Agent` 设成与取 Cookie 时同一个浏览器的 UA |
| 失效之后 | — | `cf_clearance` 过期后重新取一次（几小时到几天） |

两点实测补充：

- `turnstile_check=true` 时工具**仍会照常提交一次**（只是不重试），这样你看到的是站点自己的答复而不是我们的猜测；若该站是改了签到路径的 fork，那个自定义接口可能根本没挂 Turnstile 中间件，于是能正常签上。
- 自动求解只覆盖**阿里云** WAF 的 `acw_sc__v2`（纯字符串变换，可离线计算）。Cloudflare 的挑战 JS 是滚动混淆的，算不了，所以走上面的手工 Cookie 路线。

**日志里怎么看站点到底返回了什么？**
每个站点收尾时都会打一条 `站点签到结果`，其中 `response=` 是站点对**关键请求**（提交签到，或判定「今日已签到」的那次状态查询）的原始响应片段（截断到 160 字符、压平换行）。例如：

```
msg=站点签到结果 site=某fork站 status=success quota_awarded=0 ... message=签到成功 response="{\"message\":\"\",\"success\":true}"
msg=站点签到结果 site=标准站 status=already quota_awarded=0 ... message=今日已签到 response="{\"success\":true,\"data\":{\"stats\":{\"checked_in_today\":true,...
```

这条 `response` 只写日志（不落库、也不进 Bark 推送），用来判断「站点是回了空 message 还是明确说已签到」「额度字段叫什么」这类问题。若响应里没有这个键，说明这次没拿到可用响应（例如被拦或网络失败），此时 `message` 里会带失败详情。

**会不会重复签到？**
不会。服务端对 `(user_id, checkin_date)` 建了唯一索引，重复签到返回「今日已签到」，会被记为 `already`；本地库里每个站点每天也只有一行，重复运行是更新而不是插入（见「落库与幂等语义」）。所以叠加 `run_on_start` 补跑也不会导致重复入账。

**额度怎么换算成金额？**
显示金额 = 额度 / `quota_per_unit`（`/api/status` 返回，默认 `500000` 表示 $1）。

**如何只跑一次不常驻？**
用 `--run-once`：

```bash
go run . --run-once
# 或
make run-once
# 容器内
docker compose run --rm newapi-checkin --run-once
```

适合 cron / GitHub Actions。

> **本地运行注意**：示例配置里的 `database.path` 是容器内路径 `/data/newapi-checkin.db`，直接 `go run . --run-once` / `make run-once` 会因无法创建 `/data` 而失败。请先把 `config.yaml` 里的 `database.path` 改成 `./data/newapi-checkin.db`（或任何可写目录）；`make run-once` 会先执行 `mkdir -p data`。

**数据在哪？**
SQLite 文件位于 `data/newapi-checkin.db`（容器内为 `/data/newapi-checkin.db`，通过卷挂载到宿主机 `./data`）。主要表：

- `checkin_records`：每个站点每天一行的签到结果（当天最佳结果，重复运行只做更新）
- `run_logs`：每次批次运行的历史
- `notify_logs`：通知发送日志

## Docker

构建与运行均已真实验证（2026-09-23，本机 Docker 29.8.1 / Compose 5.5.1 / buildx 0.37.1）：

```bash
$ docker compose ps
NAME      IMAGE             SERVICE   STATUS          PORTS
newapi-checkin   newapi-checkin   newapi-checkin   Up (healthy)   127.0.0.1:8080->8080/tcp
```

> 上表 IMAGE 列是当时（镜像名还是本地 `newapi-checkin`）的输出。现在镜像名由 `docker-compose.yml` 里的 `${IMAGE_REPO:-ghcr.io/oner8/newapi-checkin}:${IMAGE_TAG:-latest}` 决定，本地 `make docker-build`（即 `docker build -t ghcr.io/oner8/newapi-checkin:latest .`）会打上 `ghcr.io/oner8/newapi-checkin:latest` 这个标签。

Dockerfile 为多阶段构建：

- 构建阶段：`golang:1.25-alpine`，`CGO_ENABLED=0`（SQLite 驱动是纯 Go，可静态编译）。
- 运行阶段：`alpine:3.20`，非 root 用户（uid 10001）、工作目录 `/app`、暴露 8080、`/data` 作为数据卷、`ENTRYPOINT ["/app/newapi-checkin"]`（无参数即常驻调度）。
- 运行阶段额外安装 `ca-certificates` 与 `tzdata`。**alpine 基础镜像两者都不带**，缺了会分别导致：所有 HTTPS 请求报 `x509: certificate signed by unknown authority`（签到与 Bark 全废）、`time.LoadLocation("Asia/Shanghai")` 报 `unknown time zone` 使配置校验失败、**容器根本起不来**。这两个坑只有真正在容器里跑才会暴露。
- healthcheck 用 busybox 的 wget：`wget -q --spider http://127.0.0.1:8080/healthz`。

可选的构建参数（`docker build --build-arg X=Y .`）：

| 参数 | 默认值 | 用途 |
| --- | --- | --- |
| `GOPROXY` | `https://goproxy.cn,direct` | 官方 `proxy.golang.org` 在部分网络下会超时，故默认国内源 |
| `GO_IMAGE` | `golang:1.25-alpine` | 构建用基础镜像，国内可换镜像站，如 `docker.m.daocloud.io/library/golang:1.25-alpine` |
| `RUNTIME_IMAGE` | `alpine:3.20` | 运行用基础镜像，同上 |
| `VERSION` | `dev` | 写入二进制的版本号（`--version` 可见）|

`docker-compose.yml` 要点：服务名与容器名 `newapi-checkin`、镜像 `${IMAGE_REPO:-ghcr.io/oner8/newapi-checkin}:${IMAGE_TAG:-latest}`（默认从 GHCR 拉，`.env` 里可用 `IMAGE_TAG` 固定版本或回滚、用 `IMAGE_REPO` 换 registry）、`env_file: .env`、`TZ=Asia/Shanghai`、挂载 `./config.yaml:/app/config.yaml:ro` 与 `./data:/data`、`ports: 127.0.0.1:8080:8080`（**只绑本机**，不暴露到局域网）、`restart: unless-stopped`、`logging` 日志轮转、healthcheck（与镜像内置的一致，可省）。它**不含 `build`**：使用者只拿这一个文件就能 `docker compose up -d`，本地开发走 `make docker-build`。

```bash
sudo install -d -o 10001 -g 10001 ./data   # 必须先建好（属主=容器内 uid 10001，见「快速开始」）

# 本地开发：用源码构建镜像后再启动
docker build -t ghcr.io/oner8/newapi-checkin:latest .
docker compose up -d

# 首次部署 / 升级：只拉镜像，不编译（镜像在 GHCR 上是公开的，无需 docker login）
docker compose pull
docker compose up -d

docker compose logs -f
```

> **Docker 部署下 `PANEL_TOKEN` 必须设置**：`POST /api/run` 在未配置令牌时「只允许 127.0.0.1」的判断依据是**请求来源 IP**，而容器看到的来源是网桥网关（例如 `172.18.0.1`），不是 `127.0.0.1`。所以即使端口只绑在本机，从宿主机 curl 触发也会被 403 拒绝。在 `.env` 里填上 `PANEL_TOKEN`，再用 `X-Admin-Token` 头调用即可（实测：正确令牌 → 202，错误/缺失 → 401）。
>
> **容器健康但站点全部 `auth_failed`**：说明凭据不被站点接受，先看下一节确定该用 Cookie 还是 PAT。

### 只用 compose 文件部署（不 clone 源码）

使用这个项目只需要一个 `docker-compose.yml`，配上自己的 `config.yaml` / `.env` / `data/`：

```text
/opt/newapi-checkin/
├── docker-compose.yml     644                 编排（复制下面那段，或用 curl 下载）
├── .env                   600                 凭据：PANEL_TOKEN / BARK_KEY / SITE_*
├── config.yaml            644                 应用配置：站点与调度
└── data/                  10001:10001         持久化：newapi-checkin.db
```

compose 里的 `./` 全部相对 **compose 文件所在目录**，所以这四样放同一层即可；放 `/opt`、`/srv` 或家目录都行，从哪个目录执行都一样（`docker compose -f /opt/newapi-checkin/docker-compose.yml up -d`）。

下面这段与仓库里的 [`docker-compose.yml`](docker-compose.yml) **完全一致，以仓库文件为准**。仓库更新后要同步，用 `curl -fsSLO https://raw.githubusercontent.com/oner8/newapi-checkin/master/docker-compose.yml` 覆盖即可——个性化内容都不在这个文件里，覆盖不会丢东西。

```yaml
services:
  newapi-checkin:
    # 这是发给使用者的样板：这一个文件 + 自己的 config.yaml / .env / data 即可运行（见 README「只用 compose 文件部署」）。
    # 镜像从 registry 拉取：首次 `docker compose up -d` 会自动拉，升级要先 `docker compose pull`。
    # 两个变量可写在 .env 里：
    #   IMAGE_TAG   固定版本 / 回滚，例如 v1.0.0（不写即 latest）
    #   IMAGE_REPO  换 registry 或镜像站，例如 registry.cn-hangzhou.aliyuncs.com/<namespace>/newapi-checkin
    image: "${IMAGE_REPO:-ghcr.io/oner8/newapi-checkin}:${IMAGE_TAG:-latest}"
    container_name: newapi-checkin
    # 凭据与令牌从 .env 注入（对应 config.yaml 里的 ${...} 引用）。程序自己不读 .env，这一项不能省。
    env_file: .env
    environment:
      TZ: Asia/Shanghai
    volumes:
      # 配置文件只读挂载（宿主上需为 644，容器内以非 root 读取）。
      # 数据库目录持久化到宿主机 ./data —— 注意该目录属主必须是容器内用户 uid 10001，
      # 否则容器无法创建 newapi-checkin.db：先执行 sudo install -d -o 10001 -g 10001 ./data
      - ./config.yaml:/app/config.yaml:ro
      - ./data:/data
    ports:
      # 只绑本机：GET /api/sites、/api/runs 不校验身份，暴露到局域网会泄露站点清单与最近签到结果。
      # 需要远程查看就放在带认证的反向代理后面（或给这两个接口也加上 admin_token）。
      - "127.0.0.1:8080:8080"
    # 让容器内能解析 host.docker.internal 指向宿主机（Linux 上用 docker 网关实现），
    # 便于把 sites[].proxy 写成 http://host.docker.internal:7890 这类形式。
    extra_hosts:
      - "host.docker.internal:host-gateway"
    restart: unless-stopped
    # 日志轮转：json-file 是默认驱动且不限大小，长期运行会把宿主机磁盘写满。
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
    healthcheck:
      # 与镜像内置的 healthcheck 相同（可省，保留也无害）。
      test: ["CMD", "wget", "-q", "--spider", "http://127.0.0.1:8080/healthz"]
      interval: 30s
      timeout: 5s
      start_period: 10s
      retries: 3
    # 可选：默认即为 bridge 网络，如需显式声明可取消注释。
    # network_mode: bridge
```

**从零到跑起来**：

```bash
mkdir -p /opt/newapi-checkin && cd /opt/newapi-checkin
B=https://raw.githubusercontent.com/oner8/newapi-checkin/master
curl -fsSLO "$B/docker-compose.yml"
curl -fsSLO "$B/config.example.yaml" && mv config.example.yaml config.yaml
curl -fsSLO "$B/.env.example"        && mv .env.example .env

vi config.yaml && vi .env        # 填站点与凭据；database.path 保持 /data/newapi-checkin.db 不要改
chmod 644 config.yaml docker-compose.yml && chmod 600 .env
sudo install -d -o 10001 -g 10001 ./data

docker compose up -d             # 首次会自动拉镜像（包是公开的，无需 docker login）
docker compose ps                # 等 STATUS 变成 Up (healthy)
curl -sS http://127.0.0.1:8080/healthz
```

**这三行分别是什么**：

| 配置 | 性质 | 作用 |
| --- | --- | --- |
| `env_file: .env` | 注入**环境变量**，不是挂载 | 程序用 `os.LookupEnv` 展开 `${SITE_XX_COOKIE}` 等，自己**不读** `.env`；`.env` 也不会出现在容器的文件系统里 |
| `./config.yaml:/app/config.yaml:ro` | **只读挂载** | 镜像里不含配置（构建时被 `.dockerignore` 排除），必须由宿主机提供；`ro` 保证容器改不了它 |
| `./data:/data` | **读写挂载** | SQLite 落在 `/data/newapi-checkin.db`，重启/升级不丢；备份就是打包这个目录 |

改完谁生效：改 `.env`（凭据、`PANEL_TOKEN`、`IMAGE_TAG`）要 `docker compose up -d`（环境变量在创建容器时注入，`restart` 不会重读）；改 `config.yaml`（站点、`app.schedule`）要 `docker compose restart`；改 `./data` 里的库不用管。

> 别把 `docker compose config` 的输出贴给别人：它会把 `.env` 里的凭据明文打印出来。只想看解析后的镜像名，用 `docker compose config --images`。

**升级**：

```bash
docker compose pull && docker compose up -d     # 只升级镜像，compose 文件不用动
```

- 固定版本 / 回滚：把 `image` 写成 `${IMAGE_TAG:-latest}`，在 `.env` 里写 `IMAGE_TAG=v1.0.0`，改完再执行上面那条。
- compose 结构变了（例如以后新增挂载）：用上面的 `curl` 覆盖一次即可，个性化内容都在 `.env` / `config.yaml` / `data/` 里。
- 直接 clone 仓库部署的人：`git pull` 即可——`.env` / `config.yaml` / `data/` 在 `.gitignore` 中，不会被覆盖。
- 本项目的约定：compose 里新增的配置项一律带 `${VAR:-默认}` 默认值，所以旧文件不同步也能照常运行；真正必须同步的变更会在本节写明。

**常见坑**：

| 现象 | 原因与做法 |
| --- | --- |
| `env file ... not found` | `.env` 必须存在（`touch .env` 也行）：它是把凭据注入容器环境的那一步 |
| 启动报 `环境变量 SITE_XX_COOKIE 未定义` | 程序只用进程环境变量展开 `${...}`，**自己不会读 `.env`**，所以别删 `env_file`；挂载一个凭据文件并不会设置环境变量 |
| 想用独立文件放凭据 | 用内置的 `<VAR>_FILE` 约定：`environment: - SITE_XX_COOKIE_FILE=/run/secrets/cookie` 并挂载该文件；此时 `.env` 里**不能**再有空的 `SITE_XX_COOKIE=`（空值也算已定义，`_FILE` 不会生效），该文件要对容器内 uid 10001 可读 |
| 容器起来就退出 / 读不到配置 | `./config.yaml` 不存在时 Docker 会创建一个**同名目录**，务必先 `cp config.example.yaml config.yaml` |
| `permission denied` 打不开数据库、反复重启 | `./data` 属主必须是 `10001:10001`（`sudo install -d -o 10001 -g 10001 ./data`） |
| `mount path must be absolute` | 卷的**容器侧**路径必须绝对（`/app/config.yaml`）；`./env:./env` 这类写法会被 daemon 拒绝 |
| `no matching manifest for linux/arm64` | 目前发布的镜像只有 `linux/amd64`；ARM 服务器需自行 `docker buildx build --platform linux/arm64 -t ghcr.io/oner8/newapi-checkin:latest --push .` |
| 从宿主机 curl `POST /api/run` 返回 403 | `.env` 里 `PANEL_TOKEN` 留空时只接受 127.0.0.1，而容器看到的来源是网桥网关；填上令牌并带 `X-Admin-Token` |

跑第二份实例（另一组站点）：把整个目录复制一份（目录名不同即可，compose 的项目名/网络名按目录名生成，不会冲突），再改 `container_name` 与端口（如 `127.0.0.1:8081:8080`）。

## 开发

```bash
make check  # 一键校验：gofmt + go vet + go test -race（与 CI 等价）
make test   # go test ./...
make vet    # go vet ./...
make fmt    # go fmt ./...
make build  # 本地编译到 bin/newapi-checkin

# 也可以直接安装（模块路径即仓库地址）
go install github.com/oner8/newapi-checkin@latest
```

目录结构：

```
.
├── main.go                     # 程序入口：解析命令行参数并启动
├── internal/
│   └── config/
│       └── config.go           # 配置加载、环境变量展开与校验
├── config.example.yaml         # 配置示例
├── .env.example                # 环境变量示例
├── Dockerfile                  # 多阶段构建
├── docker-compose.yml          # 编排文件
└── Makefile                    # 常用命令
```
