# 本地 ComfyUI（原生协议）

该目录是 BeefTV 声明式协议插件源码。后端从生成的 `comfyui-local.beeftv-plugin` 包加载，不依赖系统内置 `host:` 适配器。

本插件由 BeefTV Contributors 维护，用于适配 Comfy Org 的 ComfyUI 本地服务接口；不表示 BeefTV 与 ComfyUI 项目存在隶属、授权或合作关系。

它把**当前由 Go 硬编码实现的本机 ComfyUI 出图链路**（`backend/internal/provider/workflow/comfyui.go`）表达成声明式协议：提交宿主映射好的工作流图、轮询 `/history/{prompt_id}`、用 `/view` 取图、图生图先走 `/upload/image`、取消时补发 `/interrupt`。

完整接口见 [docs/interface.md](docs/interface.md)。

## 当前状态：源码可用，**尚未启用**

本包**没有**生成 `.beeftv-plugin`，也不应安装。本机 ComfyUI 现在仍由 Go 实现提供，已在真机验收文生图、图生图与取消。把它切到本包之前，还差三件协议层无法表达的事：

1. **outputs 的动态节点键无法枚举**。`/history` 的 `outputs` 以节点 id 为顶层键，声明式映射只能读固定键；一张图里多个 `SaveImage` 会漏产物。
2. **取消对账的四态没有声明端口**。Go 侧要先判 `execution_interrupted` 再判 `completed`，并把「既不在队列也不在 history」判为已确认；`cancelOperations` 只保证两次请求按序发出，表达不了这套判定。
3. **切流判定尚未反转**。`generation/executor.go` 目前只要 interface 属于工作流 provider 就走 Go 路径；本包的 provider id 已按目标接口填写，但在判定反转前安装它会与 Go 路径争同一个 interface。

另外两条属于**宿主要保留的接缝**，不是缺陷：工作流字段映射（含把 `/upload/image` 返回的 `subfolder/name` 回填进 `LoadImage` 节点）始终由宿主完成；`client_id` 的随机性由宿主产生。

## 前置条件

1. 本机或同一可信网络内已运行 ComfyUI，默认地址 `http://127.0.0.1:8188`。渠道 Base URL 填 ComfyUI 的**根地址**，不要带 `/prompt`、`/api` 之类路径。
2. BeefTV 默认拒绝本机、私网和链路本地上游。必须按主机名精确放行，多个主机用逗号分隔：

   ```bash
   CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS=127.0.0.1
   ```

   通过局域网主机名或反代域名访问时，只放行那一个主机名。**不要**用「允许全部私网」的写法绕过 SSRF 防护：那会让插件渠道变成对内网任意端口的读取通道。
3. 工作流必须是 **API 格式**（`{"<节点 id>": {"class_type": ..., "inputs": {...}}}`），即 ComfyUI「Save (API Format)」导出的图，不是前端带 `nodes`/`links` 的 UI 导出。
4. 反向代理挂在路径前缀下（例如 `https://gw.example.com/comfy`）时照常填写即可：本包所有端点都声明 `originPath: true`，实际请求路径会替换掉 Base URL 自带的**路径段**，只保留协议、主机和端口。代理必须允许 `POST /prompt`、`GET /history/{id}`、`GET /view`、`POST /upload/image`、`POST /queue`、`POST /interrupt` 这些根路径。

## 能力边界

- 只声明 `image` 能力，只覆盖已在真机验收过的三段链路：文生图、图生图（参考图）、取消。
- **工作流字段映射不在本包里**。`create.body.prompt` 取 `request.workflow.json`，即宿主已经映射好的图对象。分辨率、步数、采样器、scheduler、cfg、denoise、seed 这些参数如何落到 `EmptyLatentImage`/`KSampler` 的哪个字段，仍由宿主的字段映射与 `/object_info` 元数据负责；本包不解析、不改写图里任何节点。
- `create.body.client_id` 取 `request.workflow.clientId`：随机性由宿主产生，表达式语言本身没有随机数算子。
- 图生图的参考图先由 `uploads[]` 预上传到 ComfyUI 的 `input` 目录，`LoadImage.image` 的 `subfolder/name` 回填仍属于宿主映射步骤（原因见 interface.md「宿主必须保留的接缝」）。
- 拒绝视频、音频参考与蒙版：ComfyUI 侧需要对应的加载节点与 `/upload/mask` 流程，本阶段未实现，用 `validations[].assert` 在提交前显式失败，不静默丢弃素材。
- 不使用 WebSocket 进度（`/ws?clientId=`），只做 `/history` 轮询。
- 产物地址是 `baseUrl` 拼出来的 `/view` 绝对地址，`resultEphemeral: true`：宿主成功后立即下载并持久化，不把本机地址写进素材记录。

## 与 Go 实现的差异

| 环节 | Go 实现 | 本包 |
| --- | --- | --- |
| 提交 | `POST /prompt`，`{"prompt","client_id"}`，`client_id` 每次随机 16 字节 hex | 同一请求；`client_id` 透传宿主下发的 `request.workflow.clientId`，缺省即省略该键 |
| 参考图 | `LocalBytes` 取本机字节 → multipart **表单字段** `type=input`、`overwrite=true` | `uploads[]` 声明 multipart **查询参数** `type`、`overwrite`；传参位置与 Go 不同，切流前需真机复核上游读哪一侧 |
| 轮询 | `workflow.PollPolicy`：图片 1s 起、2s 间隔 | 声明 `poll.policy = {intervalSeconds:2,timeoutSeconds:1800}`；当前宿主仍按内置的 2.5s 间隔跑，未读该字段 |
| history 空对象 | Go 判 `len(payload)==0` 继续等待 | `{"$eq":[{"$len":<条目>},0]}` → 状态机映射成 `queued` |
| 产物 | 遍历 `outputs` 下**所有**节点 id 的 `images/gifs/videos/audio`，节点号按数值序稳定排列 | `outputs` 是动态键对象，表达式语言没有「取 map 全部值」的算子，只能读声明的单个 `outputNodeId` |
| 失败信息 | 汇总所有 `status.messages`，用 `；` 连接，带节点号 | 取第一条 `execution_error`/`execution_interrupted`，同样带节点号 |
| 中断判定 | `execution_interrupted` 判为已取消 | 同一判据，映射为 `cancelled` |
| 取消对账 | `GET /queue` + `GET /history` 四态判定 | 声明式协议没有取消查询端口，未表达；见缺口清单 |
| 类型校正 | INT/FLOAT/BOOLEAN 按 `/object_info` 校正后再下发 | 图已经在宿主侧映射完成，本包不做任何类型校正（原样透传） |

## 当前状态

本包是**第二阶段的目标形态**。协议层的 `uploads[]`、`poll.policy`、`request.workflow`（形状 `{json, clientId}`）与 `$get` 已经进 `internal/protocol`，但**宿主的执行链还没消费它们**：`GenerationRequest.Workflow` 没有赋值点、预上传没有调用方、轮询间隔仍取内置策略。逐条依赖与表达不了的环节列在 [docs/interface.md](docs/interface.md) 末尾。在宿主把 `request.workflow` 填上、把 `uploads[]` 跑起来之前，本包不能切流，Go 实现仍是唯一执行路径。
