# unified-api-v2

把 **WorkBuddy** 与 **豆包** 两套上游账号池收敛到**一个进程、一个端口**的 OpenAI 兼容网关。

对外只暴露「别名」，客户端不需要知道真实上游模型名；别名背后可以是单 provider，也可以是「豆包优先、失败切 WorkBuddy」的多候选自动路由。

> 本仓库只含代码与占位配置。真实的 API Key、Cookie、账号凭证一律不入库（见[敏感文件](#敏感文件绝不提交)）。

---

## 特性

| 能力 | 说明 |
|------|------|
| OpenAI 兼容 | `POST /v1/chat/completions`（含 `stream=true`）、`GET /v1/models` |
| 双上游 | `workbuddy`（OAuth 账号池）与 `doubao`（Cookie 账号池，进程内嵌入，不另起端口） |
| 别名路由 | 别名 → `providers` 数组，数组顺序即优先级；`failover` 控制失败是否切换 |
| 流式优先 | 上游首个成功字节到达即实时透传；首字节前可切换，首字节后锁定 provider |
| 会话连续性 | 同一调用方（同 API Key 或同 `X-Session-Id`）的多轮请求落在**同一个豆包会话** |
| 账号池治理 | 冷却、熔断、降级、粘性会话、账号级优先级（面板可拖拽） |
| 控制面板 | 概览 / 账号 / 号池 / 别名 / 测试台，由同一端口提供 |

---

## 环境要求

- Go **1.22.5** 或更高（`go.mod` 声明 `go 1.22.5`）
- Windows / Linux / macOS 任一
- 至少一套可用上游账号：
  - WorkBuddy：可完成 OAuth 登录的浏览器
  - 豆包：已登录 doubao.com 的浏览器 Cookie

---

## 快速开始（只接 WorkBuddy）

```bash
git clone https://github.com/xuejianqiang123/unified-api-v2.git
cd unified-api-v2

cp config.example.json config.json      # Windows: copy config.example.json config.json
```

编辑 `config.json`，至少改这三处：

| 字段 | 改成 |
|------|------|
| `api_key` | 你自己的下游密钥，建议 `sk-` 开头的随机串 |
| `listen` | 如 `127.0.0.1:8900`（示例为 `:7863`，与 docker-compose 对齐） |
| `panel.auth` | `用户名:强密码`；需要面板时把 `panel.enabled` 设为 `true` |

登录 WorkBuddy 账号（OAuth 授权后凭证落到 `auths/`）：

```bash
mkdir -p auths data
./login.sh            # 发起授权，浏览器完成登录后回终端确认
```

构建并启动：

```bash
go build -trimpath -o unified-api-v2 ./cmd/server
./unified-api-v2 -config config.json
```

冒烟验证：

```bash
curl -s http://127.0.0.1:8900/healthz
curl -s -H "Authorization: Bearer <你的 api_key>" http://127.0.0.1:8900/v1/models
curl -s -H "Authorization: Bearer <你的 api_key>" -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"只回复 OK"}],"stream":false}' \
  http://127.0.0.1:8900/v1/chat/completions
```

`/v1/models` 返回的就是别名表；请求里的 `model` 用未注册别名会得到 `400`。

---

## 接入豆包（双 provider 自动路由）

### 1. 准备豆包侧配置

```bash
mkdir -p doubao2api
cp examples/doubao.config.example.json   doubao2api/config.json
cp examples/doubao.accounts.example.json doubao2api/accounts.json
```

`accounts.json` 里把占位符替换成真实值：

| 字段 | 说明 |
|------|------|
| `name` | 账号名，面板与日志用它标识 |
| `cookie` | 登录 doubao.com 后复制的**完整 Cookie 串**（至少含 `sessionid`、`ttwid`、`s_v_web_id`、`odin_tt`、`passport_csrf_token`） |
| `device_id` / `web_id` | 与浏览器请求头一致即可，可从同一请求的查询参数或 Cookie 中取 |
| `local_app_id` | 默认 `582478`（桌面客户端标识） |
| `note` | 备注，可留空 |
| `priority` | 可选，数字越小越优先；不填则按"闲置最久"轮询 |

> Cookie 等价于账号密码。不要截图分享、不要提交、不要贴进 Issue。

### 2. 打开豆包 provider，把别名改成豆包优先

`config.json`：

```json
"providers": {
  "doubao": {
    "enabled": true,
    "config": "./doubao2api/config.json",
    "accounts": "./doubao2api/accounts.json"
  }
},
"aliases": {
  "auto": {
    "providers": ["doubao", "workbuddy"],
    "failover": true,
    "model": "auto",
    "name": "自动路由"
  }
}
```

启动时会做别名校验：**别名里出现 `doubao` 但 `providers.doubao.enabled` 为 `false`，进程直接退出并报错**——这是有意设计，避免配置拼错留到运行期。

只接豆包时，把别名写成 `"provider": "doubao"`，并接受 `auths/` 为空导致的 WorkBuddy 侧"无账号"提示。

---

## 配置参考

### 顶层字段

| 字段 | 作用 |
|------|------|
| `listen` | 监听地址，例如 `127.0.0.1:8900` |
| `api_key` | 主密钥，全权且不限分组；留空且 `api_keys` 也为空 = 不鉴权（仅限本机自用） |
| `api_keys` | 可选密钥数组（`key` / `name` / `groups`），用于多客户端分权 |
| `auth_dir` | WorkBuddy 凭证目录，默认 `./auths` |
| `state_file` | WorkBuddy 账号状态（优先级、冷却），默认 `./data/state.json` |
| `aliases` | 别名表，见下节 |
| `providers` | 各 provider 的局部配置 |
| `panel` | 控制面板 |
| `pool` | 并发上限、熔断、降级、大请求预算 |
| `session_sticky` | 粘性会话：同一会话尽量固定到同一账号 |

### 别名（`aliases`）

```json
"deepseek-v4-flash": {
  "providers": ["doubao", "workbuddy"],
  "failover": true,
  "model": "deepseek-v4-flash",
  "name": "自动路由 · DeepSeek V4 Flash",
  "context_length": 300000
}
```

| 字段 | 语义 |
|------|------|
| `provider` | 单 provider（旧格式），值 `workbuddy` 或 `doubao` |
| `providers` | 多候选，**非空时优先于 `provider`**；数组顺序即优先级 |
| `failover` | `true`：前一个候选在写出成功响应前失败，自动切下一个 |
| `model` | 上游真实模型名（豆包侧经 `model_aliases` 映射） |
| `name` | 面板展示名，可选 |
| `context_length` | 展示用上下文长度，可选 |

路由按 `providers` 顺序尝试，**配置顺序不会被健康状态重排**。因此豆包全不可用时仍会先试一次豆包再切 WorkBuddy——这是"优先豆包、失败再切"的必然代价，不是缺陷。

### 豆包 provider（`providers.doubao`）

| 字段 | 作用 |
|------|------|
| `enabled` | 是否进程内加载豆包 provider |
| `config` | 豆包侧 `config.json` 路径（字段见 `examples/doubao.config.example.json`） |
| `accounts` | 豆包账号文件路径，经 `DOUBAO2API_ACCOUNTS` 注入；为空则用豆包配置里的内联 `accounts` |

豆包侧 `upstream` 超时的真实含义：

| 字段 | 含义 | 默认 |
|------|------|------|
| `header_timeout_seconds` | 响应头（首字节）上限 | 120 |
| `idle_timeout_seconds` | 流中空闲上限，**有数据即续命** | 300 |
| `timeout_seconds` | **已失效**，聊天流不再读取；不要据此判断"5 分钟上限" | — |

客户端中断或空闲超时时，网关会**主动断开上游连接**，不留"我们走了、上游还在生成"的悬挂请求。

### 面板（`panel`）

| 字段 | 作用 |
|------|------|
| `enabled` | `true` 时在同一端口提供 `/` 与 `/api/panel/*` |
| `auth` | `用户名:密码`，表单登录换 Cookie |

面板前端不接触 API Key；"停用账号"只改运行态（`manual_disabled`），改 Cookie 才需要动 `accounts.json`。

---

## 客户端接入

| 项 | 值 |
|----|----|
| Base URL | `http://127.0.0.1:8900/v1` |
| API Key | `config.json` 的 `api_key`（`Authorization: Bearer ...`） |
| Model | 任意已注册别名，如 `auto` |

**会话连续性**：网关按 API Key 派生稳定会话标识，同一 Key 的多轮请求续写同一个豆包会话。多个互不相关的客户端共用一个 Key 时，请传 `X-Session-Id`（或请求体 `metadata.conversation_id`）隔离，否则共享上下文。

`X-Session-Id` 只影响上游会话归属，不影响鉴权。

---

## 上游额度换算参考

每日签到额度能跑多少 token，取决于两件事：**各平台倍率**和**单次签到量**。四家倍率不可直接比较——Trae / WorkBuddy / 豆包是「数值越小越省」，Loomy 的 `x0.8` / `x2.0` 是另一套量纲。

计算式：`token = 额度 × 1000 ÷ 倍率`（输入、输出合并计量）。

| 模型 | TRAE<br>100 积分/天 | WorkBuddy<br>100 积分/天 | 豆包工作<br>200 点/天 | Loomy<br>5000 积分/天 | 日合计 |
|---|---|---|---|---|---|
| **DeepSeek-V4.1-Flash**<br>原 v4-flash 位 | **1.67M**<br>0.06x 闲 | **909K**<br>0.11x | **20M** ⚠️<br>0.01x 限时 | **2.50M**<br>2.0x | **5.08M**<br>不含豆包 |
| **DeepSeek-V4-Pro** | **139K**<br>0.72x 忙 | **196K**<br>0.51x | **435K**<br>0.46x | — | **770K** |
| **GLM-5.3** | **128K**<br>0.78x | **145K**<br>0.69x | **250K**<br>0.80x | — | **523K** |
| **GLM-5.3-Flash** | **1.25M**<br>0.08x | **1.25M**<br>0.08x | **2.50M**<br>0.08x | **6.25M**<br>0.8x | **11.25M** |
| **Kimi-K3** | **54.6K**<br>1.83x | **61.7K**<br>1.62x | **135K**<br>1.48x | — | **251K** |

口径与限制，按重要性排列：

1. **绝对数字待标定**。`×1000` 是从「每百万 token ≈ 60 积分（0.06x）」反推的常数，不是任何一家官方公布的换算率。四家是否共用同一常数**未验证**。K 若为 500，表中所有数值减半。**平台间、模型间的相对关系不受影响**（K 是公共因子），绝对产能以实测标定为准。
2. **豆包 0.01x 不是结算基准**。200 点 ÷ 0.01 折合 20M token / 1.52 元，即 0.076 元每百万 token，低于上游 API 成本一个数量级以上，属拉新补贴。**该列已排除在日合计之外**，恢复后的倍率未知。
3. **表中的 token 是计费 token 总量，不是有效吞吐**。多轮 Agent 会话每轮重发完整上下文：20 轮、system prompt 5K、每轮新增 1K 的会话计费约 310K token，实际新增信息仅 25K。
4. **缓存命中不进入计分口径**。Trae 官方表述为「积分消耗 = 实际用量 × 模型倍率」，社区公式为 `(输入+输出) × 倍率 / 1000`，均不区分命中与未命中；但平台底层是否折算缓存未验证。上游 API 的缓存价差很大（DeepSeek 命中价为未命中的 1/30 ~ 1/50），因此本表适用于**容量规划**，不适用于**成本折算**。
5. **模型名映射**。上游已无 `deepseek-v4-flash`，DeepSeek 官方将该名字退役后由 V4.1-Flash 承接。前文别名示例里的 `deepseek-v4-flash` 属网关自定义别名，实际落到 V4.1-Flash，计费按 V4.1-Flash 的倍率。
6. **倍率取自客户端模型选择器（2026-11）**，随时调整；Loomy 的 Spark-X2.6 已从限免 `0x` 变为 `x2.5`，依赖「限免零成本」的规划需重新核算。

标定方法（唯一严谨路径）：单轮、固定 prompt、无历史上下文，各平台各跑一次，`K = 扣费积分 ÷ (计费 token × 倍率)`。

---

## Docker 部署

```bash
docker compose up -d --build
```

要点：

1. `docker-compose.yml` 端口固定 `7863`，与示例配置一致；改端口要同步改 compose 与 `config.json`。
2. 必须挂载 `./config.json`、`./auths`、`./data`，否则容器内读不到账号与状态。
3. 接豆包时额外挂载 `./doubao2api:/app/doubao2api`，并把 `providers.doubao.config` / `accounts` 改成容器内路径 `/app/doubao2api/config.json`、`/app/doubao2api/accounts.json`。
4. 中国大陆构建先把 `GOPROXY` 构建参数设为 `https://goproxy.cn,direct`，否则 `go mod download` 会长时间卡在第一步。
5. 容器内进程以 uid `10001` 运行；宿主机落盘的 `auths/`、`data/` 需 `chown -R 10001:10001 ./auths ./data`，否则表现为"账号数为 0"。

---

## Windows 常驻

```powershell
go build -trimpath -o unified-api-v2.exe ./cmd/server
.\unified-api-v2.exe -config config.json

schtasks /create /tn "UnifiedAPIV2" /tr "D:\path\to\unified-api-v2.exe -config D:\path\to\config.json" `
  /sc onstart /ru SYSTEM /rl HIGHEST /f
```

重启前先确认端口已释放，再启动新进程；否则会出现"启动成功但请求打不通"（旧进程仍占端口）。

---

## 敏感文件（绝不提交）

`.gitignore` 已覆盖下列路径。**若你改了存储位置，请同步改 `.gitignore`。**

| 路径 | 内容 |
|------|------|
| `config.json` | 主密钥、面板口令、豆包配置路径 |
| `keys.json` | 下游密钥存储，首次启动自动生成，含明文 `sk-` |
| `auths/` | WorkBuddy OAuth 凭证 |
| `data/` | 账号状态、审计日志 |
| `doubao2api/accounts.json` | 豆包 Cookie（等价账号密码） |
| `*.log` / `usage*.jsonl` | 请求明细、上游账号名 |
| `*.exe` / `*.exe~` | 构建产物与编辑器备份 |

提交前自查：

```bash
git status --short
git diff --cached --name-only | grep -E "config.json|keys.json|auths|accounts.json"          # bash
git diff --cached --name-only | Select-String "config.json|keys.json|auths|accounts.json"    # PowerShell
```

---

## 故障排查

| 症状 | 原因 | 处理 |
|------|------|------|
| 启动即退出，提示"别名指向 doubao，但 providers.doubao.enabled 未开启" | 别名与 provider 开关不一致 | 打开 `providers.doubao.enabled`，或从别名删掉 `doubao` |
| `400` 且提示未注册别名 | `model` 用了上游模型名或拼错 | 使用 `/v1/models` 返回的别名 |
| `503` 所有账号均不可用 | Cookie 过期、账号冷却、WorkBuddy 凭证失效 | 面板看账号状态；更新 `accounts.json` 或重新 `login.sh` |
| `401` / 鉴权失败 | `api_key` 不匹配，或未带 `Bearer` | 核对密钥；确认启动日志 `api_key=true` |
| 首字节等 60s 以上 | 豆包冷启动 | 保持流式；不要把 `header_timeout_seconds` 压得过小 |
| 流式在 5 分钟整被截断 | 跑的是旧二进制（`Client.Timeout` 总时长） | 重新构建；启动日志应含 `outbound timeout: total=off` |
| 豆包界面仍是一串互不相干的对话 | 修复前生成的孤立会话，无法事后合并 | 之后的新请求按 Key 聚合；需隔离就传 `X-Session-Id` |
| 面板 404 | `panel.enabled=false` | 改为 `true` 并重启 |

---

## 仓库结构

```
cmd/server/          统一网关入口（唯一需要部署的二进制）
internal/server/     OpenAI 兼容层、别名路由、面板、会话标识
internal/dbserver/   豆包 provider（进程内嵌入）
internal/doubao*/    豆包协议与配置
internal/upstream/   WorkBuddy 上游客户端
internal/pool/       WorkBuddy 账号池
examples/            上游配置模板（占位符，可直接复制）
cmd/{login,credit,signin,trial,acct,stats,activity}/  历史子工具，非部署必需
```

---

## 合规与免责

- 本项目仅用于**个人学习与研究**，不提供绕过付费、批量注册或规避上游风控的能力。
- 使用上游账号即表示你自行承担与上游服务条款、账号安全相关的一切后果；仓库维护者不提供账号、不代登录。
- 请勿将其部署为面向公众的商用服务，也不要索取或分享他人的 Cookie / API Key。
- 仓库内不含任何真实凭证；`examples/` 中所有值均为占位符。

License 见 [LICENSE](LICENSE)。
