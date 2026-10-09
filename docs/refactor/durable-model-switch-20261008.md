# Durable 会话换模型失效（本地复现，已修）

Windows 宿主机开发形态实测，上游同步到 `4b60dd4`（v1.7.13 之后）时仍能稳定复现；修复已落地，下面的复现步骤、判据与四组归因表保留作为对照基线。

## 现象（修复前）

已有 durable 会话在助手模型被更换后，每一轮都以 `status=unanswered`、`reason=no_model` 结束；同时 `/api/assistant/status` 与 `/api/assistant/health` 仍返回 `available: true` 并显示新模型。界面表现为「这一步没有完成 / 这一轮没有全部完成」，用户看不出需要新建会话。当前行为见「修复」一节。

## 判据

`@earendil-works/pi-durable/dist/harness/generation.js`：回合 checkpoint 保存 `model: {provider, modelId}`，恢复与请求准备时都走 `runtime.models.getModel(ref.provider, ref.modelId)`，解析不到即调用 `failNoModel`，把该回合永久置为 unanswered。`agent-host/server.mjs` 每次启动只注册当前配置的那一个模型 id，所以旧引用在新宿主里必然解析失败。修复前这条会话引用没有任何入口被改写；现在宿主在接管会话时按官方接口改绑，见「修复」一节。

## 归因实验（2026-10-08，修复前基线）

服务：`llama-server`（Qwen3.6-27B @8299、MiniCPM5-1B @8298，`-c 32768 --jinja`）、`cmd/server` @8090（`CANVAS_BACKEND_DATA_DIR=.local/project-workbench-debug`、`CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS=127.0.0.1`）、owner 中继 @8091。渠道 `local-accept` 的 `modelProfiles[].protocol` 为 `chat-completion`。同一画布、同一会话、消息固定「只回复两个字：收到」。

| 组 | 操作 | 结果 |
| --- | --- | --- |
| A1 | 27B 新建 durable 会话首轮 | `done`，回复「收到」 |
| A2 | **同模型**仅 `POST /assistant/host/restart`，复用 A1 会话 | `done`，回复「收到」 |
| B1 | 换成 1B 并重启宿主，复用 A1 会话 | `unanswered`，`reason=no_model` |
| B2 | 同一时刻新建会话（仍是 1B） | `done`，回复「收到」 |

结论：触发条件是**会话建立后更换模型**，宿主重启本身无害（与上游 `durable-host-verification-20261008.md` 的同模型崩溃恢复测试不矛盾——那条路径不换模型 id）。修复后同一组操作里 A2 仍为 `done`，B1 也应为 `done`（宿主把这条会话改绑到当前模型），只有改绑发生前已经建立 checkpoint 的那一轮会给出 `session_model_changed`。

## 修复（已落地）

**宿主侧改绑** — `agent-host/durable-session-owner.mjs`：

- `open(canvasId, sessionId, adopt)` 新增 `adopt` 参数（`:130`），只有 `createSession`（`:232`）和 `activateSession`（`:246`）传 `true`；`history()`（`:491`）不传，因此读取历史不会写会话文档。
- adopt 时在 `harness.root(...)` 之后调用 `adoptModel(entry, configured)`（`:101`，调用点 `:213`），用官方 `entry.root.configure({ model: configured }, CTX)` 把这条会话的模型引用改成当前配置的模型。必须走 `configure`：官方 `root()` 只在 root 会话缺失时应用 `agent` 配置，已存在的会话不会因此换模型（判据见 `agent-host/node_modules/@earendil-works/pi-durable/dist/harness/types.d.ts` 的 `AgentChange.model`，它落在可回放的 `pi.agent` 文档里）。生成阶段每轮从该 agent 解析模型，所以改绑对后续轮次生效。
- 因此不需要原先设想的「把当前模型 id 注册成旧引用的别名」或「重建会话搬运历史」——官方接口本身就能改这条会话的引用。

**在途轮次**（`:452`-`:461`）：某一轮在改绑之前已经把自己的 `{provider, modelId}` 写进 `request`/`poll` 阶段的 checkpoint，官方任务没有公开接口能改写那份 checkpoint，它只能以裸 `no_model` 结算。宿主改为记住离开的引用（`entry.modelSwitch`），把这一轮的结算原因报成 `session_model_changed`，`error` 正文换成「创作助手已改用 `<新 modelId>`，但这一轮仍绑定在宿主不再提供的 `<provider>/<modelId>` 上，因此没有执行。请重新发送这条要求。」；任一后续轮次以 `done` 结束后清除该标记（`:458`），不再重复提示。

**Go 侧状态口径** — `sessionWarning` 的观测边界：

- `backend/internal/assistant/provider.go`：`:24` 契约常量 `ReasonSessionModelChanged = "session_model_changed"`（不得改名），`:28` 固定中文 `SessionModelChangedMessage`，`:32` `HostTurnReasonMessage` 只认这一条，未知原因返回 false，不把宿主正文透出去。
- `backend/internal/handler/assistant_session_warning.go`：按 **用户 + 画布** 的有界内存观测——TTL 30 分钟（`:16`）、容量 256（`:18`）、到顶丢最旧、进程重启即失效。它既不是权限也不是持久状态，不能用来判定「这条会话能不能用」。
- 观测来源只有两处，都不额外请求宿主：`backend/internal/handler/agent_proxy.go` 转发 `/assistant/chat` 时读 `turn_end` 的 `errorReason`（只观测，不改写转发给浏览器的原文），以及宿主直接拒绝这次对话时的 `reason`（原样透传，`msg` 用上面那句）。
- `GET /assistant/status` 只在 `available:true`、请求带 `canvasId`、这个免身份接口能解析出当前用户、且该画布确实观测到过该原因时，让 `data` 额外带 `sessionWarning: "session_model_changed"`；缺任一条就不带该字段，`available` 不受影响。这里刻意不按早期草稿那样把状态报成不可用（当时拟用 `reason=session_model_unavailable`）：`available:false` 会让同一次判定把新建对话一起拦住，而宿主和当前模型其实是好的，坏的只有这条旧会话。
- 清除时机：`POST /assistant/sessions` 成功新建会话即清除该画布的提示；`POST /assistant/sessions/activate` 不清除（切回钉死旧模型的会话时提示可能仍然成立）；后续任一轮次以非该原因的终态结算也会清除。

**界面** — `web/src/services/api/agent-assistant.ts` 增加 `session_model_changed` 分支与 `AgentHostStatus.sessionWarning`，`getAgentHostStatus(canvasId?)` 带上画布；`canvas-assistant-copy.ts` 的 `assistantSessionNotice()` 只在 `available === true` 且提示值命中时出话术，未知值一律不出（不把后端原文透给用户）；`canvas-assistant-sidebar.tsx` 复用既有 `.canvas-assistant-notice` 渲染这条**非阻断**提示，「新建对话」接到既有 `assistant.startNewSession`，不参与 `composerDisabled` 判定；新建会话成功后立即复查一次状态，不让提示停留到下一次轮询。回合级「继续」入口仍不对这条原因开放（`assistantTurnCanContinue` 未列入），因为话术要求的是重新发送原要求，而不是让模型「继续完成上一条」。

## 回归覆盖

原「验收口径」要求的 A2 与 B1 已落成用例，另外补了在途 checkpoint 一条：

- `agent-host/durable-session-owner.test.mjs`（真实宿主）：
  - `restarting the host keeps an existing durable session usable, with the same and with a switched model` —— 覆盖 A2 同模型重启不退化，以及 B1 换模型后旧会话仍然可用。
  - `an in-flight request checkpoint on a withdrawn model settles as session_model_changed and the session recovers` —— 在途轮次结算为 `session_model_changed`，且后续轮恢复正常。
- `backend/internal/handler/assistant_session_model_changed_test.go`：`TestAssistantChatRejectionKeepsSessionModelChangedReasonAndActionableMessage`（拒绝时 reason 不被白名单改写、msg 可行动）、`TestAssistantSessionModelChangedWarningTracksTurnOutcome`（提示随回合终态记与清）、`TestAssistantUnavailableStatusDoesNotReportSessionWarning`（不可用时不带提示）、`TestAssistantSessionWarningStoreScopesAndExpiry`（按用户与画布隔离、过期失效）。
- `backend/internal/handler/assistant_runtime_test.go` 的 `TestTerminalLineRequiresMatchingBusinessTurn` 扩了 `errorReason` 断言。
- 契约同步在 `backend/internal/handler/openapi.yaml`（`canvasId` 查询参数、`sessionWarning?`、`turn_end.errorReason`）。

**文案口径**：这条原因只影响改绑前那**一轮**，旧对话本身在改绑后可以继续，所以界面话术不能写成「这条对话无法继续」。后端固定说明、前端 `agentAssistantFailureText` 分支、`http-api.mdx` 与 `openapi.yaml` 用的是同一句：「这一轮开始时用的是另一个模型，创作助手没有执行它。现在已改用当前模型，重新发送这条要求即可继续；也可以新建对话，历史仍然可以查看。」宿主侧那条 `error` 正文（带新旧 modelId）比它更具体，两者指向同一个行动。

本机 Windows 实跑结果：

- `agent-host`：`.tools/bun-1.4.2/bun.exe test`（先按 `agent-host/bun.lock` 装冻结依赖）全量 **117 pass / 2 fail**，上面两条新增真实宿主用例都在通过集合里；剩下的 2 个失败名（`真实宿主：SIGTERM 释放活动会话后退出`、`指针目录不可写时 replace 失败…`）是本机既有平台噪音，且失败名集合是本机基线的子集（基线里的 `server exits when PORT and LISTEN_FD are missing` 本轮通过）。
- `backend`：`go build ./...`、`go vet ./internal/assistant ./internal/handler`、`gofmt -l` 全干净；`go test ./internal/assistant` ok；`TestTerminalLineRequiresMatchingBusinessTurn` 与 `TestAssistantSessionWarningStoreScopesAndExpiry` 通过。另三条 handler 用例的失败全部发生在 `t.TempDir()` 清理阶段的 `unlinkat ... assistant.db ... used by another process`，断言本身没有失败——同文件里改动前就存在的 `TestAssistantStatusReportsResolvedModelAndReasons` 等既有用例以完全相同的方式失败，属「跨平台测试基线」记录的既有噪音。
- `web`：`bun run build`（`tsc --noEmit` + `vite build`）EXIT=0；`assistant-session-model-changed.test.ts`、`canvas-agent-assistant-wiring.test.ts`、`assistant-readiness.test.ts` 合计 19 pass / 0 fail。

## 桌面打包形态验收

`scripts/build-beeftv-windows-release.ps1` 在 Windows 本机原生构建（`go1.26.7` + `C:\msys64\mingw64\bin\gcc.exe` CGO，随包 Node 24.15.0），产出 `backend\cmd\desktop\build\bin\BeefTV.exe`（v1.7.13）、`agent-host\runtime\node.exe`、87 个官方 `*.beeftv-plugin` 与 `cli\beeftv.exe`。随包的 `agent-host\durable-session-owner.mjs` 与仓库内文件 SHA-256 一致，即改绑逻辑确实进了包。

用**独立数据目录**启动该 exe（不指向 `%APPDATA%\BeefTV`）后的实测：

- 窗口正常渲染（WebView2 子进程在位，首页左下版本角标 v1.7.13）；不带 `X-Desktop-Token` 的 `/api/assistant/status` 返回 403，入口守卫在包内生效。
- owner 凭据下 `/api/ops` 给出 27 个操作，`/api/plugins` 为 89 个 `source=official` 且 `enabled`，插件确实从 exe 同目录加载。
- `/api/assistant/status` 从 `model_not_configured` 变为 `available: true`：宿主由**随包 Node** 拉起，不依赖系统 Node。
- 换模型回归：同一条 durable 会话先在 MiniCPM5-1B 上跑一轮（回复 `received`），把助手模型换成 Qwen3.6-27B 并 `POST /api/assistant/host/restart`，**同一 sessionId** 的下一轮正常回复「收到」，`errorReason` 为空；`/api/assistant/status?canvasId=...` 不带 `sessionWarning`。打包形态的改绑因此不再需要新建对话。
- 凭据路径：桌面默认数据目录是 `%APPDATA%\BeefTV`。本次把数据目录放到非系统卷时，实测 `agent_owner_token` 的 DACL 继承 `NT AUTHORITY\Authenticated Users:(I)(M)` 与 `BUILTIN\Users:(I)(RX)`，即那里的「0600」并不成立——与 AGENTS.md 的既有结论一致，冒烟数据目录已在验收后删除。

## 仍未验证

- 真实付费模型渠道下的换模型行为：本轮取证来自本地推理服务与测试宿主。
- `chatgpt-subscription` 渠道：账号隔离头只在该协议下由 `applyAssistantSubscriptionHeaders` 注入，只能用真实订阅验证。
- NSIS 安装包：该脚本只产出可执行目录，不产出安装器，安装/升级路径未在包内验证。
- 上游反馈仍未提单：向 `glanderness/BeefTV` 或 pi-durable 说明「会话钉死模型引用 + status 仍 available」这一组合（本表四组数据可直接作为复现步骤）；改绑走 `configure` 而不是 `root()` 也值得回给上游确认。

## 相关本地事实

- `cmd/server` 形态必须写数据目录下的 `agent_config.json` 指定宿主命令，否则 `host_command_missing`；桌面形态才走随包 Node。
- 助手提示 + 26 个操作约 15k tokens，本地推理服务 8192 上下文会直接溢出为 `model_error`。
- 账号隔离头只在渠道协议为 `chatgpt-subscription` 时由 `applyAssistantSubscriptionHeaders` 注入，普通渠道的 `headers` 不进宿主出站，因此该项只能用真实订阅验证。
