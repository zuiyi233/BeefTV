# Durable 会话换模型失效（本地复现，未修）

Windows 宿主机开发形态实测，上游同步到 `4b60dd4`（v1.7.13 之后）之后仍然存在。

## 现象

已有 durable 会话在助手模型被更换后，每一轮都以 `status=unanswered`、`reason=no_model` 结束；同时 `/api/assistant/status` 与 `/api/assistant/health` 仍返回 `available: true` 并显示新模型。界面表现为「这一步没有完成 / 这一轮没有全部完成」，用户看不出需要新建会话。

## 判据

`@earendil-works/pi-durable/dist/harness/generation.js`：回合 checkpoint 保存 `model: {provider, modelId}`，恢复与请求准备时都走 `runtime.models.getModel(ref.provider, ref.modelId)`，解析不到即调用 `failNoModel`，把该回合永久置为 unanswered。`agent-host/server.mjs` 每次启动只注册当前配置的那一个模型 id，所以旧引用在新宿主里必然解析失败。

## 归因实验（2026-10-08）

服务：`llama-server`（Qwen3.6-27B @8299、MiniCPM5-1B @8298，`-c 32768 --jinja`）、`cmd/server` @8090（`CANVAS_BACKEND_DATA_DIR=.local/project-workbench-debug`、`CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS=127.0.0.1`）、owner 中继 @8091。渠道 `local-accept` 的 `modelProfiles[].protocol` 为 `chat-completion`。同一画布、同一会话、消息固定「只回复两个字：收到」。

| 组 | 操作 | 结果 |
| --- | --- | --- |
| A1 | 27B 新建 durable 会话首轮 | `done`，回复「收到」 |
| A2 | **同模型**仅 `POST /assistant/host/restart`，复用 A1 会话 | `done`，回复「收到」 |
| B1 | 换成 1B 并重启宿主，复用 A1 会话 | `unanswered`，`reason=no_model` |
| B2 | 同一时刻新建会话（仍是 1B） | `done`，回复「收到」 |

结论：触发条件是**会话建立后更换模型**，宿主重启本身无害（与上游 `durable-host-verification-20261008.md` 的同模型崩溃恢复测试不矛盾——那条路径不换模型 id）。

## 修复方向（待定，尚未动代码）

1. 宿主侧优先：`agent-host` 在启动或配置变更时，把当前模型 id 注册为会话引用的别名，或在 `ensureSession` 解析失败时按官方 API 重建会话并搬运历史，使旧对话继续可用。
2. Go 侧兜底：`ResolveAssistantProvider` 已经拿到会话与配置的模型，若宿主回报 `no_model` 应把 `/assistant/status` 从 `available: true` 改为明确的不可用原因（例如 `reason=session_model_unavailable`），不要在状态里继续宣称可用。
3. 界面：助手面板在该原因下给出「新建会话继续」的入口，而不是笼统的「这一步没有完成」。
4. 上游：向 `glanderness/BeefTV` 或 pi-durable 反馈「会话钉死模型引用 + status 仍 available」的组合；本表四组数据可直接作为复现步骤。

验收口径：新增回归应同时覆盖 A2（同模型重启后旧会话仍可用）与 B1（换模型后旧会话要么可用、要么给出可行动原因），并断言 `status` 与 `chat` 的一致性。

## 相关本地事实

- `cmd/server` 形态必须写数据目录下的 `agent_config.json` 指定宿主命令，否则 `host_command_missing`；桌面形态才走随包 Node。
- 助手提示 + 26 个操作约 15k tokens，本地推理服务 8192 上下文会直接溢出为 `model_error`。
- 账号隔离头只在渠道协议为 `chatgpt-subscription` 时由 `applyAssistantSubscriptionHeaders` 注入，普通渠道的 `headers` 不进宿主出站，因此该项只能用真实订阅验证。
