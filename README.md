# unified-api-v2

把 **WorkBuddy、豆包、TRAE、华为云 CodeArts（码道）、Loomy** 五个平台的上游账号收敛到**一个进程、一个端口**的 OpenAI 兼容网关。

客户端只认三件套 `base_url + api_key + model`；「用哪个平台、哪个账号、失败切谁」全部收在服务端，`model` 只写别名。

- **适用场景**：多个 AI 客户端共用一套上游账号；多平台账号池 + 自动故障切换；单机自用，需要面板看账号状态、额度与请求日志。
- **不适用**：面向公众的多租户商用服务；绕过上游付费或风控的用途（见[合规与免责](#12-合规与免责)）。

**部署流程（7 步，只接 WorkBuddy 约 10 分钟）**

| # | 做什么 | 验证方式 |
|---|--------|----------|
| 1 | 装 Go 1.26+（接 TRAE 再装 Python 3.11+） | `go version` |
| 2 | 拿代码、生成 `config.json` | 文件存在且三项必改已改 |
| 3 | 编译网关与 `login` 工具 | 两个可执行文件产出 |
| 4 | 启动进程 | 日志出现 `listening on ...`；`/healthz` 有账号后为 200 |
| 5 | 面板接上游账号 | 账号表出现对应行 |
| 6 | 配别名（约 5 秒自动热载） | `/v1/models` 列出别名 |
| 7 | 客户端接入；需要常驻再注册计划任务 | 一次真实对话返回 200 |

---

## 1. 前置条件

### 1.1 软件依赖

| 依赖 | 版本 | 必需性 | 用途 |
|------|------|--------|------|
| Git | 任意 | 必装 | 拉代码 |
| Go | **1.26+** | 源码部署必装 | 编译网关与 `login` 工具；以 `go.mod` 的 `go 1.26.0` 为准 |
| 浏览器 | Chrome / Edge | 必装 | OAuth 授权、打开面板 |
| Python | 3.11+ | **仅接 TRAE** | 运行 `trae2api` 反向代理 |
| Docker | 24+ | 可选 | 容器部署（容器内只含网关本体） |

操作系统：Windows 10/11（功能完整，本文命令以此为主）、Linux、macOS。Linux/macOS 能运行网关与 WorkBuddy / 豆包 / TRAE 的凭据导入，但依赖本机客户端或 Windows 凭据存储的「导入本机」类功能不可用（详见 [4.0](#40-总览)）。

### 1.2 端口

| 端口 | 用途 | 默认值来源 | 能否改 |
|------|------|-----------|--------|
| 8900 | 网关 + 控制面板 | `config.json` → `listen` | 可，改完重启 |
| 8000 | `trae2api`（仅接 TRAE） | `trae/run.py` | 可，需同步 `providers.trae.base_url` |
| 8765 | Trae 网页授权助手（仅接 TRAE） | `trae/.env` → `WEB_LOGIN_LISTENER_PORT` | 可 |

### 1.3 上游账号

至少准备一个，注册与取凭据的完整路径见[第 4 章](#4-接入上游平台)。没有账号时网关能启动，但对话请求会返回 `503`。

### 1.4 目录与磁盘

`auths/`、`loomy/`、`codearts/` 在首次写入凭证时自动创建；`data/` 建议在启动前建好（[步骤 4](#步骤-4启动) 已包含）；`doubao2api/`、`trae/data/` 按对应平台章节创建。预留约 200 MB（二进制 + 依赖；Trae 虚拟环境另占约 60 MB）。

---

## 2. 配置文件

### 2.1 生成

```powershell
copy config.example.json config.json
```

```bash
# Linux / macOS
cp config.example.json config.json
```

### 2.2 三项必改

| 字段 | 示例值 | 说明 |
|------|--------|------|
| `listen` | `127.0.0.1:8900` | 只允许本机访问用 `127.0.0.1:`；要让局域网客户端连，用 `0.0.0.0:8900` 或 `:8900`，并务必配强 `api_key` |
| `api_key` | `sk-3f9c1d7a5b2e48c6a0d9f4e7b1c8a2d5` | 下游密钥，客户端用它鉴权。留空 = 不鉴权，仅限本机自用 |
| `panel.auth` | `admin:Str0ng-Pass-2026` | 面板登录口令，格式 `用户名:密码`；**不要保留示例值** |

> 示例里的 `admin:CHANGE_ME_BEFORE_START` 是公开占位符。原样启动、且 `listen` 绑在 `0.0.0.0` 时，面板等同于对局域网敞开——先改口令再启动。

生成随机密钥：

```powershell
# PowerShell
-join ((1..48) | ForEach-Object { '0123456789abcdef'[(Get-Random -Max 16)] })
```

```bash
# Linux / macOS
openssl rand -hex 24
```

### 2.3 最小可用配置（只接 WorkBuddy）

```json
{
  "listen": "127.0.0.1:8900",
  "api_key": "sk-CHANGE_ME",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "panel": { "enabled": true, "auth": "admin:CHANGE_ME" },
  "providers": {
    "doubao":   { "enabled": false, "config": "./doubao2api/config.json", "accounts": "./doubao2api/accounts.json" },
    "trae":     { "enabled": false, "base_url": "http://127.0.0.1:8000", "api_key": "" },
    "codearts": { "enabled": false, "accounts": "codearts/accounts.json", "takeover": false }
  },
  "aliases": {
    "auto": { "providers": ["workbuddy"], "failover": true, "model": "auto", "name": "自动路由" }
  }
}
```

`config.example.json` 是完整版（含池、排程、告警等全部可选段），可直接复制后按需删减。**未启用的 provider 不加载、不占端口**；但别名里出现的 provider 必须已启用，否则进程启动即退出（见[第 7 章](#7-常见报错)）。

### 2.4 打开更多平台

`workbuddy` 无开关（本地凭证池恒启用）；`loomy` 无开关（凭证缺失不阻止启动，请求时回 `503`）；其余三家按需打开：

| 平台 | 配置段 | 关键字段 |
|------|--------|----------|
| 豆包 | `providers.doubao` | `enabled`、`config`、`accounts` |
| TRAE | `providers.trae` | `enabled`、`base_url`、`api_key`（对应 `trae2api` 的 `RELAY_API_KEYS`，留空 = 本机免鉴权） |
| CodeArts | `providers.codearts` | `enabled`、`accounts`、`takeover` |

### 2.5 别名与路由

```json
"aliases": {
  "deepseek-v4-flash": {
    "providers": ["doubao", "workbuddy", "trae", "codearts", "loomy"],
    "failover": true,
    "model": "deepseek-v4-flash",
    "model_by_provider": {
      "trae": "deepseek-v4.1-flash",
      "loomy": "deepseek-v4-flash-1017"
    },
    "name": "DeepSeek V4 Flash"
  }
}
```

| 字段 | 语义 |
|------|------|
| `provider` | 单 provider（旧格式），值为 `workbuddy` / `doubao` / `trae` / `loomy` / `codearts` |
| `providers` | 多候选，**非空时优先于 `provider`**；数组顺序即优先级 |
| `failover` | `true`：前一个候选在写出成功响应前失败，自动切下一个 |
| `model` | 上游真实模型名，各平台不一致时必须写 `model_by_provider` |
| `model_by_provider` | 按平台覆盖上游模型名（如 TRAE 叫 `deepseek-v4.1-flash`） |
| `require` | 能力下限：`{"context": 300000, "images": true}`，不满足的候选项在分发前跳过 |
| `context_length` | 仅面板展示用，可选 |

路由按 `providers` 顺序尝试，**不会因健康状态重排**。豆包全不可用时仍会先试一次豆包再切下一家，这是「优先豆包、失败再切」的必然代价。

---

## 3. 部署步骤

### 步骤 1：获取代码

```bash
git clone https://github.com/xuejianqiang123/unified-api-v2.git
cd unified-api-v2
```

**预期结果**：目录内含 `go.mod`、`config.example.json`、`cmd/server`。

### 步骤 2：生成并修改配置

```powershell
copy config.example.json config.json
```

编辑 `config.json`，至少改 [2.2](#22-三项必改) 的三项。

**预期结果**：`config.json` 内 `api_key` 不再是 `sk-CHANGE_ME_BEFORE_START`，`panel.auth` 不再是 `admin:CHANGE_ME_BEFORE_START`。

### 步骤 3：编译

```powershell
go build -trimpath -o unified-api-v2.exe ./cmd/server
go build -trimpath -o login.exe ./cmd/login
```

```bash
# Linux / macOS
go build -trimpath -o unified-api-v2 ./cmd/server
go build -trimpath -o login ./cmd/login
```

| 产物 | 作用 | 缺失后果 |
|------|------|----------|
| `unified-api-v2(.exe)` | 网关主程序 | 无法启动 |
| `login(.exe)` | WorkBuddy OAuth 工具 | 面板「OAuth 登录」报 `oauth_unavailable` |

`login` 必须与网关**同目录**：面板按进程所在目录查找它。Windows 下文件名是 `login.exe`，Linux/macOS 是 `login`（网关按平台自动匹配）。

**预期结果**：两个文件生成，`go build` 无报错。国内网络若卡在下载依赖，先设 `go env -w GOPROXY=https://goproxy.cn,direct`。

### 步骤 4：启动

```powershell
mkdir auths, data -Force
.\unified-api-v2.exe -config config.json
```

```bash
mkdir -p auths data
./unified-api-v2 -config config.json
```

**预期结果**（日志逐行出现）：

```
loomy provider 已接线（会话存储 loomy/conversations.json，已有 0 条）
全局别名已启用：N 条（只暴露别名，未注册别名返回 400）
unified-api-v2 listening on 127.0.0.1:8900 (api_key=true, aliases=N)
```

`api_key=true` 表示鉴权已生效（`api_key` 非空）。

出现 `别名 "x" 指向 doubao，但 providers.doubao.enabled 未开启` 属于配置不一致，按提示修正后重启。

### 步骤 5：基础验证

```bash
curl -s http://127.0.0.1:8900/healthz
curl -s -H "Authorization: Bearer <你的 api_key>" http://127.0.0.1:8900/v1/models
```

| 请求 | 预期结果 |
|------|----------|
| `GET /healthz` | 有可用账号时 HTTP 200；**刚部署、还没接账号时返回 503**（`healthy=0`，属预期，不是启动失败） |
| `GET /v1/models` | 返回别名列表；带错 Key 返回 `401` |
| `POST /v1/chat/completions` | 账号可用时 200；无可用账号时 `503` |

`/healthz` 的判据是「现在能不能受理请求」，不是「进程是否活着」：进程正常但号池为空时返回 503。接入账号后再看，`healthy` 应大于 0。

`/v1/models` 返回的就是可用 `model` 值；请求里写未注册别名会得到 `400 unknown_model`。

### 步骤 6：打开面板

浏览器访问 `http://127.0.0.1:8900/`，用 `panel.auth` 的用户名密码登录。

**预期结果**：进入「概览」，能看到 Provider 与路由、实时使用；左侧导航含「上手引导」，逐平台列出注册与导入步骤。

### 步骤 7：接入上游账号

按[第 4 章](#4-接入上游平台)逐平台操作。**预期结果**：面板「账号」页出现对应 provider 的行。

### 步骤 8：配置别名

按 [2.5](#25-别名与路由) 写好别名，保存即可：网关每 5 秒检查 `config.json`，**别名表自动热载**，无需重启。`listen`、`providers`、`panel`、`api_key` 这类改变服务形态的字段仍需重启。

**预期结果**：约 5 秒后 `/v1/models` 出现新别名；启动日志出现 `配置热载：别名 +N -M ~K`。JSON 写坏时网关保留旧配置并在日志报 `解析失败，保留旧配置`，不会带着半截配置运行。

### 步骤 9：客户端接入

见[第 5 章](#5-客户端接入)。

---

## 4. 接入上游平台

### 4.0 总览

| 平台 | 注册 / 邀请入口 | 凭据形态 | 导入方式 | 独立进程 |
|------|----------------|----------|----------|----------|
| WorkBuddy | <https://www.workbuddy.cn/events/invite?inviteCode=aop0fmuxb> | OAuth 令牌 | 面板 OAuth / `login.exe` | 否 |
| 豆包 | <https://www.doubao.com/pc/desktop-fission/pro-trial/invited?inviteCode=v10zWpgbBZ> | Cookie | 面板粘贴 / 导入本机客户端 | 否（进程内） |
| TRAE | <https://www.trae.cn/events/code-fission/9AAHWWJUQUDX?utm_source=copy_link&utm_medium=code_fission> | Cloud-IDE-JWT + refresh | 面板导入本机 / 粘贴令牌 / 网页授权 | **是**（`:8000`） |
| CodeArts（码道） | <https://codearts.huaweicloud.com/> | STS 四件套 + DPoP 私钥 | 面板从 IDE 导入 / `codearts-import` | 否（进程内） |
| Loomy | <https://loomy.xunfei.cn/web> | `loomy_web_session` Cookie | `tools\loomy-login.ps1` + 面板导入 | 否（进程内） |

以上链接含邀请码，注册即计入邀请；CodeArts 与 Loomy 无邀请码，走官方入口。

**导入方式的系统限制**：带「导入本机」的动作（豆包、Trae、CodeArts、Loomy）依赖本机客户端与 Windows 凭据存储（DPAPI、Edge profile、`%APPDATA%`），**仅在 Windows 可用**。Linux/macOS 可走：WorkBuddy OAuth、豆包「粘贴 Cookie」、Trae「粘贴凭证」或网页授权；CodeArts 的凭据只能从码道 IDE 导入（非 Windows 环境请改用其他平台）。

### 4.1 WorkBuddy

**注册**：打开邀请链接，用手机号或邮箱登录网页版即完成注册。国内版与国际版是两套账号体系，OAuth 时选对应 realm。

**取凭据**：不需要手抄。OAuth 授权完成后由网关落盘到 `auths/workbuddy-<uid>.json`。

**导入（推荐，面板）**

1. 面板 →「账号」→ `WorkBuddy ▾` →「OAuth 登录」
2. Realm 选 `cn`（国内）或 `global`（海外）→「获取登录链接」
3. 浏览器打开链接并完成登录
4. 回到弹窗点「完成登录」

**预期结果**：提示「登录成功，已添加账号：<uid>」，约 6 秒后账号表出现新行。

**导入（命令行）**

```bash
./login.exe --realm=cn url     # Windows；Linux/macOS 去掉 .exe
# 浏览器打开该 URL 完成登录，然后：
./login.exe --realm=cn poll
```

**生效与验证**：网关每 5 秒扫描 `auths/`；面板账号行出现即已加载。用 `model=auto` 发一次对话验证。

**注意**：面板 OAuth 依赖同目录的 `login.exe`（非 Windows 为 `login`）；缺失时报 `oauth_unavailable`，补[步骤 3](#步骤-3编译)即可。

### 4.2 豆包

**注册**：打开邀请链接，用手机号登录，可选用网页版或桌面客户端。

**取凭据（两条路，任选）**

| 方式 | 前置条件 | 操作 |
|------|----------|------|
| A. 导入本机（推荐） | 本机安装并登录豆包桌面客户端；**导入前完全退出客户端**（Cookie 库有文件锁） | 面板 →「账号」→ `豆包 ▾` →「导入本机豆包」 |
| B. 粘贴 Cookie | 浏览器已登录 `doubao.com` | 面板 →「账号」→ `豆包 ▾` →「粘贴 Cookie」，填账号名 + 完整 Cookie |

Cookie 至少包含 `sessionid`、`ttwid`、`s_v_web_id`、`odin_tt`、`passport_csrf_token`。**Cookie 等价于账号密码**，不要截图、不要提交、不要贴进 Issue。

**首次准备豆包侧配置**

```bash
mkdir -p doubao2api
cp examples/doubao.config.example.json   doubao2api/config.json
cp examples/doubao.accounts.example.json doubao2api/accounts.json
```

`config.json` 打开 provider：

```json
"providers": {
  "doubao": { "enabled": true, "config": "./doubao2api/config.json", "accounts": "./doubao2api/accounts.json" }
}
```

**验收**：面板账号表出现豆包行（含积分列）；`model` 用豆包支持的别名发一次对话返回 200。

**注意**：豆包冷启动首字节可能超过 60 秒，属上游特性；不要把 `header_timeout_seconds` 压得太小。

### 4.3 TRAE

**注册**：打开邀请链接，下载并登录 Trae CN 客户端（也可走网页授权）。TRAE 账号体系区分 CN 与国际版，本项目按 CN 配置。

**架构**：TRAE 不是进程内 provider，需要独立的 Python 进程 `trae2api`（已 vendor 在 `trae/`）。网关只做 HTTP 转发，账号、OAuth、风控仍归 `trae2api` 管。

**步骤 1：准备 Python 环境**

```powershell
cd trae
python -m venv .venv
.\.venv\Scripts\python.exe -m pip install -r requirements.txt -r requirements-native.txt
copy .env.example .env
```

**步骤 2：启动 `trae2api`**

```powershell
.\run.cmd          # 无窗口后台启动（pythonw），日志写 trae\data\server.err.log
```

**预期结果**：`curl http://127.0.0.1:8000/healthz` 返回 200。

**步骤 3：导入账号（三选一）**

| 方式 | 前置条件 | 操作 |
|------|----------|------|
| A. 导入本机 Trae | 本机 Trae CN 客户端已登录 | 面板 →「账号」→ `Trae ▾` →「导入本机 Trae」 |
| B. 粘贴令牌 | 手上有 Cloud-IDE-JWT | 面板 → `Trae ▾` →「粘贴凭证」 |
| C. 网页授权 | 能运行本机授权助手 | 打开 `http://127.0.0.1:8000/web/login`，按页面提示下载并运行授权助手 |

方式 C 的授权助手是 Trae 授权页限制导致的必需组件（回调必须落在 `127.0.0.1`），不是本项目缺陷。

**步骤 4：网关接线**

```json
"providers": {
  "trae": { "enabled": true, "base_url": "http://127.0.0.1:8000", "api_key": "" }
}
```

`api_key` 需与 `trae/.env` 的 `RELAY_API_KEYS` 一致；两边都留空即本机免鉴权。

**验收**：面板账号表出现 Trae 行（含积分）；用映射到 TRAE 的别名（如 `deepseek-v4-flash` → `deepseek-v4.1-flash`）发一次对话返回 200。

**注意**：`run.py` 已固定 `UPSTREAM_MODE=ide`（本机 Trae CN 账号在 remote 通道下无模型权限），不要覆盖；TRAE 的模型名与别家不同，务必用 `model_by_provider` 映射。

### 4.4 CodeArts（华为云码道）

**注册**：打开 <https://codearts.huaweicloud.com/>，用华为云账号登录，下载并安装码道 IDE，在 IDE 内登录一次。

**取凭据**：凭据由 IDE 写入 `%APPDATA%\codearts\secretStorage`（DPAPI + OSCrypt 加密），网关只读复用，不需要手工抄写。

**导入**

| 方式 | 命令 / 操作 |
|------|------------|
| 面板 | 「账号」→ `CodeArts ▾` →「从本机 IDE 导入」 |
| 命令行 | `.\codearts-import.exe`（先 `go build -o codearts-import.exe ./cmd/codearts-import`） |
| 只查剩余时长 | `.\codearts-import.exe -check` |

**验收**：面板账号表出现 CodeArts 行与额度；`codearts-import.exe -check` 显示 STS 剩余时间。

**注意（重要）**

- STS 临时凭证约 **24 小时**有效，过期后请求返回 `upstream_credential_expired`，开一次 IDE 让凭据续期，再执行一次导入即可，**不需要重启网关**（每次请求重读 `codearts/accounts.json`）。
- `takeover=false`（默认）：网关只读复用，IDE 自己续期，IDE 不会掉登录。
- `takeover=true`：网关自己持有并续期 refresh_token，代价是 IDE 那份失效（下次打开 IDE 需重新登录）。除非确实不打算再用 IDE，否则保持 `false`。
- 必须在**同一 Windows 用户**下导入（DPAPI 与用户绑定）；换用户或换机器需重新登录 IDE。

### 4.5 Loomy

**注册**：打开 <https://loomy.xunfei.cn/web>，用手机号 + 验证码登录。

**导入**

```powershell
.\tools\loomy-login.ps1
```

1. 脚本会打开一个**独立 Edge 窗口**（专用 profile，不影响日常浏览器）
2. 在窗口内用手机号 + 验证码登录
3. **关闭整个窗口**（不是最小化；Windows 会锁 Cookies 文件）
4. 面板 →「账号」→ `Loomy ▾` →「导入本机 Loomy」

**验收**：面板账号表出现 Loomy 行；用它支持的别名发一次对话返回 200。

**注意**：Loomy 只签发一个 `loomy_web_session` Cookie，没有 refresh token，失效后只能重走一次登录 + 导入；账号存 `loomy/accounts.json`，导入会更新同名 `local` 账号，其他账号保留。

---

## 5. 客户端接入

| 项 | 值 |
|----|----|
| Base URL | `http://127.0.0.1:8900/v1`（局域网客户端改成宿主机 IP） |
| API Key | `config.json` 的 `api_key`，请求头 `Authorization: Bearer <key>` |
| Model | 任意已注册别名，如 `auto`、`deepseek-v4-flash` |

```bash
curl -s http://127.0.0.1:8900/v1/chat/completions \
  -H "Authorization: Bearer <你的 api_key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"只回复 OK"}],"stream":false}'
```

**会话连续性**：网关按 API Key 派生稳定会话标识，同一 Key 的多轮请求续写同一个上游会话。多个互不相关的客户端共用一个 Key 时，请传 `X-Session-Id`（或请求体 `metadata.conversation_id`）隔离，否则会共享上下文。`X-Session-Id` 只影响会话归属，不影响鉴权。

**多客户端分权**：`api_keys` 数组可为每个客户端发独立密钥并绑定分组：

```json
"api_keys": [
  { "key": "sk-client-a", "name": "cherry", "groups": ["default"] },
  { "key": "sk-client-b", "name": "chatbox", "groups": [] }
]
```

`groups` 为空等价主密钥（不限分组）。

---

## 6. 常驻运行

### 6.1 Windows 一键脚本

```powershell
.\deploy.ps1                    # 编译 → 准备 Trae → 重启（不改计划任务）
.\deploy.ps1 -RegisterTasks     # 额外注册/更新两个开机自启任务
.\deploy.ps1 -SkipTrae          # 只编译网关
.\deploy.ps1 -MigrateData       # 从旧的 ..\trae2api\data 迁移账号与缓存
```

脚本特性：幂等（重复执行不重复装依赖、不覆盖已有 `data/`）；**默认不碰计划任务**，必须显式 `-RegisterTasks`；失败即停，不做「看起来成功」的兜底。

**预期结果**：末尾打印 `完成。面板: http://127.0.0.1:8900/`，并逐项报告网关与 Trae 的 `healthz` 状态。

注册的计划任务：

| 任务名 | 实际命令 | 说明 |
|--------|----------|------|
| `UnifiedAPIV2` | `cmd /c run-unified-v2.cmd` | 包装脚本负责工作目录、日志重定向 |
| `Trae2API` | `cmd /c trae\run.cmd` | 无窗口启动 `trae2api` |

不要绕过包装脚本直接注册 exe，否则会丢工作目录与日志重定向。

### 6.2 手工注册计划任务

```powershell
schtasks /create /tn "UnifiedAPIV2" /tr "D:\path\to\run-unified-v2.cmd" `
  /sc onstart /ru SYSTEM /rl HIGHEST /f
```

重启前先确认端口已释放，否则会出现「启动成功但请求打不通」（旧进程仍占端口）。`deploy.ps1` 已按端口精确定位旧进程，手工操作时用：

```powershell
Get-NetTCPConnection -LocalPort 8900 -State Listen | Select-Object OwningProcess
Stop-Process -Id <PID>
```

### 6.3 Docker

```bash
docker compose up -d --build
```

| 要点 | 说明 |
|------|------|
| 端口 | compose 固定 `8900:8900`，与 `config.example.json` 的 `listen` 一致；改端口要同步改 compose 与 `config.json` |
| 必须挂载 | `./config.json`、`./auths`、`./data`，否则容器内读不到账号与状态 |
| 接豆包 | 额外挂载 `./doubao2api:/app/doubao2api`，并把 `providers.doubao.config` / `accounts` 改成容器内路径 |
| 接 TRAE | 容器内**不含** `trae2api`；让它留在宿主机，`providers.trae.base_url` 填 `http://host.docker.internal:8000`（Linux 需加 `extra_hosts`） |
| 国内构建 | 先设 `GOPROXY=https://goproxy.cn,direct`，否则 `go mod download` 会长时间卡住 |
| 文件属主 | 容器内进程以 uid `10001` 运行；宿主机执行 `chown -R 10001:10001 ./auths ./data`，否则表现为「账号数为 0」 |

---

## 7. 常见报错

| 症状 | 原因 | 处理 |
|------|------|------|
| 启动即退出：`别名 "x" 指向 doubao，但 providers.doubao.enabled 未开启` | 别名与 provider 开关不一致 | 打开对应 `providers.*.enabled`，或从别名里删掉该 provider |
| 启动即退出：`别名 "x" 的 provider "y" 未知` | provider 名拼错 | 只允许 `workbuddy` / `doubao` / `trae` / `loomy` / `codearts` |
| `400 unknown_model` | `model` 用了上游模型名或拼错 | 用 `/v1/models` 返回的别名 |
| `401` / 鉴权失败 | `api_key` 不匹配，或没带 `Bearer` | 核对密钥；确认启动日志里 `api_key=true` |
| `503` 所有账号均不可用 | Cookie 过期、账号冷却、WorkBuddy 凭证失效 | 面板看账号状态；更新 Cookie 或重新 OAuth |
| 面板 404 | `panel.enabled=false` | 改为 `true` 并重启 |
| 面板反复弹登录框 | 旧二进制（Basic Auth 弹窗） | 重新构建；新版本走 `/login` 表单登录 |
| 面板「OAuth 登录」报 `oauth_unavailable` | 网关同目录没有 `login.exe` | 执行 `go build -o login.exe ./cmd/login` |
| 豆包导入本机失败（提示文件被占用） | 豆包客户端仍在运行 | 完全退出豆包客户端（含托盘）后重试 |
| 豆包首字节等 60 秒以上 | 豆包冷启动 | 保持流式；不要把 `header_timeout_seconds` 压得过小 |
| 流式在 5 分钟整被截断 | 跑的是旧二进制（`Client.Timeout` 总时长） | 重新构建；启动日志应含 `outbound timeout: total=off` |
| TRAE 请求 `502` / `503` | `trae2api` 未启动或 `base_url` 错 | `curl http://127.0.0.1:8000/healthz`；看 `trae\data\server.err.log` |
| TRAE 面板「导入本机 Trae」失败 | 本机 Trae CN 未登录，或装在非默认路径 | 先在本机 Trae 客户端登录；或改用「粘贴凭证」 |
| CodeArts `503 upstream_credential_expired` | STS 临时凭证过期（约 24h） | 打开码道 IDE 让凭据续期，再执行一次导入 |
| CodeArts 导入失败：`本机没有找到已登录的码道凭据` | IDE 未登录，或换了 Windows 用户 | 用导入时同一用户登录 IDE 后重试 |
| Loomy `loomy_profile_missing` | 未运行登录脚本 | 先执行 `.\tools\loomy-login.ps1` |
| Loomy `登录窗口` 关闭后仍读不到 Cookie | 窗口未完全关闭，或只打开了页面没登录 | 完全关闭窗口；确认已登录成功再导入 |
| 端口占用 / 启动成功但请求打不通 | 旧进程仍监听端口 | 按 [6.2](#62-手工注册计划任务) 按端口结束旧进程 |
| `/healthz` 一直 503，但进程在跑 | 号池里没有可用账号（`healthy=0`） | 先接账号；看面板账号表是否为空或全部冷却 |
| 改了别名但 `/v1/models` 没变 | 文件没保存、JSON 写坏，或改的是 `providers` 段（别名才热载） | 看启动日志：`解析失败，保留旧配置` / `配置热载：别名 +N`；provider 变更需重启 |
| Docker 内账号数为 0 | `auths/`、`data/` 属主不是 10001 | `chown -R 10001:10001 ./auths ./data` |
| Docker 构建卡在 `go mod download` | 官方 Go 代理不可达 | 传 `GOPROXY=https://goproxy.cn,direct` |
| 计划任务启动后无日志、请求不通 | 直接注册了 exe，丢了工作目录 | 改注册 `run-unified-v2.cmd` / `trae\run.cmd` |

---

## 8. 配置参考

### 8.1 顶层字段

| 字段 | 作用 | 默认 |
|------|------|------|
| `listen` | 监听地址 | 代码缺省 `:7863`；示例配置为 `:8900` |
| `api_key` | 主密钥，全权且不限分组；与 `api_keys` 同时为空 = 不鉴权 | 空 |
| `api_keys` | 分组密钥数组（`key` / `name` / `groups`） | 空 |
| `auth_dir` | WorkBuddy 凭证目录 | `./auths` |
| `state_file` | 账号状态（优先级、冷却） | `./data/state.json` |
| `aliases` | 别名表，非空即启用统一模式 | 空 |
| `providers` | 各 provider 局部配置 | 全部关闭 |
| `panel` | 控制面板 | `enabled=false` |
| `pool` | 并发上限、熔断、降权、快过期窗口 | 见 8.4 |
| `session_sticky` | 粘性会话 | `enabled=true`，`ttl=30m` |
| `cooldown` | 软限流冷却基数与封顶 | `600s` / `2h` |
| `schedule` | 定时任务（签到、活跃、保活等） | 见 `config.example.json` |
| `upstream` | 出站超时与客户端指纹 | 见 8.5 |
| `admin` / `metrics` / `alerting` / `budget` | 运维面，默认全关 | 关闭 |

### 8.2 豆包 provider（`providers.doubao`）

| 字段 | 作用 |
|------|------|
| `enabled` | 是否进程内加载豆包 provider |
| `config` | 豆包侧 `config.json` 路径（字段见 `examples/doubao.config.example.json`） |
| `accounts` | 豆包账号文件路径，经 `DOUBAO2API_ACCOUNTS` 注入；为空则用豆包配置里的内联 `accounts` |

豆包侧 `upstream` 超时的真实含义：

| 字段 | 含义 | 默认 |
|------|------|------|
| `header_timeout_seconds` | 响应头（首字节）上限 | 120 |
| `idle_timeout_seconds` | 流中空闲上限，有数据即续命 | 300 |
| `timeout_seconds` | **已失效**，聊天流不再读取；不要据此判断「5 分钟上限」 | — |

客户端中断或空闲超时时，网关会主动断开上游连接，不留悬挂请求。

### 8.3 面板（`panel`）

| 字段 | 作用 |
|------|------|
| `enabled` | `true` 时在同一端口提供 `/`、`/login` 与 `/api/panel/*` |
| `auth` | `用户名:密码`，表单登录换 Cookie；Basic Auth 仍兼容脚本调用 |

面板前端不接触 API Key；「停用账号」只改运行态（`manual_disabled`），改 Cookie 才需要动账号文件。

### 8.4 账号池（`pool`）

| 字段 | 作用 | 默认 |
|------|------|------|
| `max_in_flight` | 单账号并发上限 | 3 |
| `max_in_flight_global` | 国际版账号并发上限 | 2 |
| `breaker_threshold` / `breaker_cooldown` | 连续失败熔断阈值与冷却 | 3 / `30m` |
| `degrade_threshold` / `degrade_cooldown` | 连败降权阈值与冷却 | 5 / `10m` |
| `selection_mode` | `weighted`=加权随机；`priority`=严格按账号优先级 | `weighted` |
| `expiring_soon` | 快过期积分窗口，用于优先消耗 | `168h` |
| `idle_weight_per_hour` / `idle_weight_max` | 闲置加权 | 0.5 / 5.0 |

### 8.5 出站（`upstream`）

| 字段 | 作用 | 默认 |
|------|------|------|
| `timeout_seconds` | 单请求总超时 | 120 |
| `header_timeout_seconds` | 首字节上限，缺省回落 `timeout_seconds` | 同上 |
| `idle_timeout_seconds` | 流空闲上限，有数据即续命 | 300 |
| `client_name` | 出站指纹身份，`SaaS` 还原旧行为 | `WorkBuddy` |
| `passthrough_ip` | 是否透传客户端 IP | `false` |

### 8.6 环境变量覆盖

以下常用变量优先于 `config.json`（便于容器部署）：`WB2A_LISTEN`、`WB2A_API_KEY`、`WB2A_AUTH_DIR`、`WB2A_STATE_FILE`、`WB2A_TIMEOUT_SECONDS`、`WB2A_HEADER_TIMEOUT_SECONDS`、`WB2A_IDLE_TIMEOUT_SECONDS`、`WB2A_PROMPT_MODE`、`DOUBAO2API_ACCOUNTS`。

---

## 9. 敏感文件（绝不提交）

`.gitignore` 已覆盖下列路径。**若改了存储位置，请同步改 `.gitignore`。**

| 路径 | 内容 |
|------|------|
| `config.json` | 主密钥、面板口令、provider 路径 |
| `keys.json` | 豆包 provider 的下游密钥存储，含明文 `sk-` |
| `auths/` | WorkBuddy OAuth 凭证 |
| `data/` | 账号状态、审计日志 |
| `doubao2api/accounts.json` | 豆包 Cookie（等价账号密码） |
| `loomy/accounts.json` | Loomy 会话 Cookie |
| `codearts/accounts.json` | 华为云 STS 四件套 + DPoP 私钥 |
| `trae/.env`、`trae/data/` | Trae 令牌、账号与缓存 |
| `*.log` / `usage*.jsonl` | 请求明细、上游账号名 |
| `*.exe` / `*.exe~` | 构建产物与编辑器备份 |

提交前自查：

```bash
git status --short
git diff --cached --name-only | grep -E "config.json|keys.json|auths|accounts.json"          # bash
git diff --cached --name-only | Select-String "config.json|keys.json|auths|accounts.json"    # PowerShell
```

---

## 10. 仓库结构

```
cmd/server/            统一网关入口（唯一必须部署的二进制）
cmd/login/             WorkBuddy OAuth 工具（面板 OAuth 依赖）
cmd/codearts-import/   CodeArts 凭据导入
cmd/codearts-checkin/  CodeArts 手动签到
internal/server/       OpenAI 兼容层、别名路由、故障切换、面板、会话标识
internal/dbserver/     豆包 provider（进程内）
internal/doubao*/      豆包协议与配置
internal/traecn/       本机 Trae 凭证解密（面板「导入本机 Trae」）
internal/codearts/     华为云签名、凭据、续期、签到
internal/loomy/        Loomy provider 与会话存储
internal/pool/         账号池（冷却、熔断、降权、粘性）
internal/scheduler/    定时任务调度
examples/              上游配置模板（占位符，可直接复制）
trae/                  vendor 的 Trae2api（独立 Python 进程）
tools/                 Loomy 登录脚本、无窗口启动器
docs/                  设计说明与平台接入清单（不入版本库）
```

---

## 11. 本次文档改动点

相对上一版 README，本次改动如下（便于对照旧文档）：

1. **补全 provider 覆盖**：原文只写 WorkBuddy + 豆包，实际代码已支持 TRAE、CodeArts、Loomy；新增第 4 章逐平台「注册 → 取凭据 → 导入 → 验证 → 注意」。
2. **修正版本要求**：Go 由 `1.22.5` 改为 **1.26+**，与 `go.mod`（`go 1.26.0`）和 Dockerfile（`golang:1.26-alpine`）一致。
3. **端口统一为 8900**：示例配置、`docker-compose.yml`、`Dockerfile` 健康检查统一为 8900（原 7863 仅 Docker 使用），避免「按示例启动却和脚本/自检端口不一致」。
4. **新增可执行步骤与判据**：每一步给出可直接复制的命令与预期结果；新增 `login.exe` 编译说明（面板 OAuth 的前置条件，原文缺失）。
5. **新增面板引导页**：面板左侧「上手引导」逐平台列出注册链接（含邀请码）、导入入口与验证方法。
6. **错误表扩充**：从 8 条扩到 23 条，覆盖 TRAE 子进程、CodeArts 凭据过期、Loomy 文件锁、Docker 属主、计划任务工作目录等实际故障。
7. **结构调整**：按「前置条件 → 配置 → 步骤 → 接平台 → 客户端 → 常驻 → 报错 → 参考」重排；额度换算表移入附录，不再打断部署主线。

---

## 12. 合规与免责

- 本项目仅用于**个人学习与研究**，不提供绕过付费、批量注册或规避上游风控的能力。
- 使用上游账号即表示你自行承担与上游服务条款、账号安全相关的一切后果；仓库维护者不提供账号、不代登录。
- 请勿将其部署为面向公众的商用服务，也不要索取或分享他人的 Cookie / API Key。
- 仓库内不含任何真实凭证；`examples/` 中所有值均为占位符。

License 见 [LICENSE](LICENSE)。

---

## 附录 A：上游额度换算参考

每日签到额度能跑多少 token，取决于**各平台倍率**与**单次签到量**。四家倍率不可直接比较——TRAE / WorkBuddy / 豆包是「数值越小越省」，Loomy 的 `x0.8` / `x2.0` 是另一套量纲。

计算式：`token = 额度 × 1000 ÷ 倍率`（输入、输出合并计量）。

| 模型 | TRAE<br>100 积分/天 | WorkBuddy<br>100 积分/天 | 豆包工作<br>200 点/天 | Loomy<br>5000 积分/天 | 日合计 |
|---|---|---|---|---|---|
| **DeepSeek-V4.1-Flash**<br>原 v4-flash 位 | **1.67M**<br>0.06x 闲 | **909K**<br>0.11x | **20M** ⚠️<br>0.01x 限时 | **2.50M**<br>2.0x | **5.08M**<br>不含豆包 |
| **DeepSeek-V4-Pro** | **139K**<br>0.72x 忙 | **196K**<br>0.51x | **435K**<br>0.46x | — | **770K** |
| **GLM-5.3** | **128K**<br>0.78x | **145K**<br>0.69x | **250K**<br>0.80x | — | **523K** |
| **GLM-5.3-Flash** | **1.25M**<br>0.08x | **1.25M**<br>0.08x | **2.50M**<br>0.08x | **6.25M**<br>0.8x | **11.25M** |
| **Kimi-K3** | **54.6K**<br>1.83x | **61.7K**<br>1.62x | **135K**<br>1.48x | — | **251K** |

口径与限制，按重要性排列：

1. **绝对数字待标定**。`×1000` 是从「每百万 token ≈ 60 积分（0.06x）」反推的常数，不是任何一家官方公布的换算率。四家是否共用同一常数**未验证**。K 若为 500，表中所有数值减半；平台间、模型间的相对关系不受影响（K 是公共因子），绝对产能以实测标定为准。
2. **豆包 0.01x 不是结算基准**。200 点 ÷ 0.01 折合 20M token / 1.52 元，即 0.076 元每百万 token，低于上游 API 成本一个数量级以上，属拉新补贴。该列已排除在日合计之外，恢复后的倍率未知。
3. **表中的 token 是计费 token 总量，不是有效吞吐**。多轮 Agent 会话每轮重发完整上下文：20 轮、system prompt 5K、每轮新增 1K 的会话计费约 310K token，实际新增信息仅 25K。
4. **缓存命中不进入计分口径**。TRAE 官方表述为「积分消耗 = 实际用量 × 模型倍率」，社区公式为 `(输入+输出) × 倍率 / 1000`，均不区分命中与未命中；平台底层是否折算缓存未验证。上游 API 的缓存价差很大（DeepSeek 命中价为未命中的 1/30 ~ 1/50），因此本表适用于**容量规划**，不适用于**成本折算**。
5. **模型名映射**。上游已无 `deepseek-v4-flash`，DeepSeek 官方将该名字退役后由 V4.1-Flash 承接。别名表里的 `deepseek-v4-flash` 属网关自定义别名，实际落到 V4.1-Flash，计费按 V4.1-Flash 的倍率。
6. **倍率取自客户端模型选择器（2026-11）**，随时调整；Loomy 的 Spark-X2.6 已从限免 `0x` 变为 `x2.5`，依赖「限免零成本」的规划需重新核算。

标定方法（唯一严谨路径）：单轮、固定 prompt、无历史上下文，各平台各跑一次，`K = 扣费积分 ÷ (计费 token × 倍率)`。
