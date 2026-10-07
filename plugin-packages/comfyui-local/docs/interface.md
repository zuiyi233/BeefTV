# 本地 ComfyUI 接口字段

## 协议身份

- 插件 ID：`comfyui-local`。
- Provider ID：`comfyui-workflow-image`。
- 能力：`image`。
- 默认 Base URL：`http://127.0.0.1:8188`。
- 鉴权驱动：`none`（ComfyUI 原生接口没有鉴权，宿主不为该渠道注入任何凭据头）。
- 创建：`POST /prompt`。
- 查询：`GET /history/{{taskId}}`。
- 结果：`GET /view`（由响应映射拼成绝对地址，不声明独立结果端点）。
- 取消：`POST /queue`，随后 `cancelOperations` 补发 `POST /interrupt`。

所有端点都声明 `originPath: true`：ComfyUI 的原生接口全部挂在服务根路径下，而用户填写的 Base URL 可能带反向代理前缀。`originPath` 让声明的路径替换掉 Base URL 的路径段，只保留协议、主机和端口。

## 前置条件与 SSRF

BeefTV 默认拒绝本机、私网和链路本地上游。渠道指向 `127.0.0.1:8188` 时，部署环境必须**按主机名精确放行**：

```bash
CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS=127.0.0.1
```

局域网或反代场景同样只放行那一个主机名，多个用逗号分隔。不要设置「允许全部私网」来绕过 SSRF 防护：声明式渠道同样受出站校验保护，绕过它等于把内网任意端口暴露成可被模型渠道驱动的出口。

## 配置字段

无。`configuration.fields` 是空数组——ComfyUI 原生协议没有 API Key，`auth.type` 为 `none`。地址、工作流 JSON 和字段映射都来自渠道本身的配置，不由本包声明。

## 统一字段映射

| 字段 | 类型 | 必填 | 来源 | 用途 |
| --- | --- | --- | --- | --- |
| `request.workflow.json` | object | 是 | 宿主映射 | 已映射好的 API 格式工作流图，整体作为 `/prompt` 的 `prompt` 字段。 |
| `request.workflow.clientId` | string | 否 | 宿主映射 | `/prompt` 的 `client_id`；未提供时省略该键。 |
| `request.images[]` | media[] | 否 | 生成请求 | `uploads[]` 的迭代源，逐项预上传。 |
| `request.providerOptions["comfyui-local"].outputNodeId` | string | 否* | 渠道扩展 | 指明从 `outputs` 的哪个节点取产物。*image 链路实际必需。 |

工作流节点内的业务参数（提示词、`KSampler.steps/sampler_name/scheduler/cfg/denoise/seed`、`EmptyLatentImage.width/height/batch_size`、`LoadImage.image`）**不通过 manifest 声明**：字段映射本身仍是宿主职责，manifest 只描述「怎么把已映射好的图交给 `/prompt`」。本包 `parameters` 里列出这些名字，是为了让渠道配置与画布控件有可枚举的口径，`mapping` 一律写明是宿主映射目标。

`steps`、`cfg`、`denoise`、`seed` 落到图里必须保持 JSON 数字与布尔类型：`/prompt` 按 `/object_info` 校验输入类型，INT/FLOAT 收到字符串会整张图 400 拒绝。这个类型校正发生在宿主映射，本包不做二次校正。

## 上游请求模板逐字段清单

### create

| 位置 | 值 |
| --- | --- |
| `method` | `POST` |
| `path` | `/prompt` |
| `contentType` | `application/json` |
| `originPath` | `true` |
| `body.prompt` | `{"$ref":"request.workflow.json"}` |
| `body.client_id` | `{"$omitEmpty":{"$ref":"request.workflow.clientId"}}` |

`request.workflow.json` 缺失时 `$ref` 得到 nil，`prompt` 键会被整段丢弃；提交前的 `validations` 已先断言图非空，不会把空 body 发给上游。

### uploads（预上传，图生图必需）

| 位置 | 值 |
| --- | --- |
| `id` | `referenceImage` |
| `foreach` | `{"$ref":"request.images"}` |
| `operation.method` | `POST` |
| `operation.path` | `/upload/image` |
| `operation.contentType` | `multipart/form-data` |
| `operation.originPath` | `true` |
| `operation.files[0]` | `{"name":"image","source":{"$ref":"item.dataUrl"},"filename":{"$ref":"item.name"}}` |
| `operation.query` | `{"type":{"$literal":"input"},"overwrite":{"$literal":"true"}}` |
| `capture` | `{"name":{"$ref":"response.name"},"subfolder":{"$ref":"response.subfolder"},"type":{"$ref":"response.type"}}` |

逐项迭代 `request.images`，当前元素绑定为 `item`，位置绑定为 `index`。`capture` 的三个字段在 `create`/`poll`/`result` 的求值环境里以 `uploads.referenceImage` 暴露，是**与输入顺序对齐的数组**。

> 传参位置差异：Go 实现把 `type=input`、`overwrite=true` 写成 multipart **表单字段**，本包按既定契约写成 **查询参数**。两者是同一接口的不同传参位置，切流前必须真机复核上游读取哪一侧。

### poll

| 位置 | 值 |
| --- | --- |
| `method` / `path` | `GET` / `/history/{{taskId}}` |
| `contentType` | `application/json` |
| `originPath` | `true` |
| `policy` | `{"intervalSeconds":2,"timeoutSeconds":1800}` |

### cancel 与 cancelOperations

| 顺序 | 端点 | body |
| --- | --- | --- |
| `cancel` | `POST /queue` | `{"delete":[{"$ref":"taskId"}]}` |
| `cancelOperations[0]` | `POST /interrupt` | `{"prompt_id":{"$ref":"taskId"}}` |

两段都发是刻意的：`/queue` 的 `delete` 只摘掉排队项、对执行中的任务无效；执行中的任务要 `/interrupt`。任务已结束时两者都不产生副作用，因此不需要先查状态。`/interrupt` 必须显式带 `prompt_id`——缺该字段会**全局中断**，在共享实例上会误伤他人任务。

本包**不声明 `result` 端点**：宿主的 `BuildResult` 只能产出一个二进制请求，且其求值环境只有 `{request, taskId}`、没有 `response`，拿不到 `/view` 需要的 `filename`/`subfolder`/`type`，也无法一次取多张产物。结果一律通过 `response.images` 给出绝对地址，由宿主逐条下载持久化。

## Provider 扩展键

- `providerOptions.comfyui-local.outputNodeId`

`client_id` 不属于扩展键，它随 `request.workflow.clientId` 由宿主下发。

## 响应映射逐字段清单

| 位置 | 声明 |
| --- | --- |
| `taskIdPaths` | `["prompt_id"]`（取 `/prompt` 的同步响应） |
| `resultKind` / `resultEphemeral` | `image` / `true` |
| `status` | `$switch` 状态机，见下 |
| `message` | `$map` 第一条 `execution_error`/`execution_interrupted` 消息，带节点号 |
| `images` | `$map` 产物 → `{"$ref":"baseUrl"}` 拼绝对 `/view` 地址 |

未声明 `errorPaths`：`/history` 的失败信息在 `{{taskId}}` 这个运行时顶层键之下，静态点分路径无法定位，失败语义完全由 `status` 状态机承担。

状态机按顺序判定，解决 `/history` 的几种响应形态：

| 上游形态 | 判定表达式 | 映射状态 |
| --- | --- | --- |
| 空对象 `{}` 或无本任务条目 | `{"$eq":[{"$len":<条目>},0]}` | `queued` → 继续轮询，**不算失败** |
| `status.messages` 含 `execution_interrupted` | `$filter` 命中数 > 0 | `cancelled` |
| `status.status_str == "error"` | `$lower/$trim/$toString` 后比较 | `failed` |
| `status.completed == true` | `$toBool` | `succeeded` |
| `outputs[<节点>]` 条目非空（该节点有任意输出键） | `$len` > 0 | `succeeded` |
| 条目已出现但未执行完 | 默认分支 | `executing` |

`$len` 对 map 返回键数，因此「history 里还没有本任务条目」可以判成排队中，而不是被当成空结果失败；被 `/interrupt` 结束的任务同样写入 history 且 `status_str` 为 `error`，只能靠 `execution_interrupted` 消息与真正的执行失败区分。

最后一条兜底用 `$len(outputs[<节点>]) > 0` 而不是只数 `images`：`SaveAnimatedWEBP`/`SaveVideo` 把结果写在 `gifs`/`videos`/`audio` 下，只认 `images` 会让这类工作流停在 `executing` 直到超时。代价是节点条目形如 `{"images": []}` 时也会判成 `succeeded`，此时产物为空，宿主以「声明式协议已完成但没有返回媒体地址」失败——与 Go 的「ComfyUI 任务完成但没有返回产物」同一类终态，不会静默产出空素材。

取产物时，`outputs` 的顶层键是节点 id（动态），必须先用 `$get` 按 `taskId` 取条目、再取 `outputs`、再按声明的 `outputNodeId` 取节点，最后读 `images`/`gifs`/`videos`/`audio` 四种键并用 `$concatArrays` 合并。每个文件拼成：

```text
{baseUrl}/view?filename={urlencode filename}&subfolder={urlencode subfolder}&type={urlencode (type ?? output)}
```

`$urlencode` 与 Go 的 `url.Values.Encode()` 逐字节一致（空格转 `+`，`#`、`%`、`&`、`/` 全部转义），`subfolder` 含 `/` 或空格时不能被当路径分段处理；`subfolder` 缺失时编码成空串，与 Go 的 `query.Set("subfolder","")` 行为一致。缺 `filename` 的条目由 `$if` 丢弃，避免拼出 `/view?filename=` 这种伪地址。

`resultEphemeral: true`：`/view` 是本机地址，换端口或换设备即失效，宿主必须在成功后立即下载并持久化成素材，不把本机地址留在结果里。

## 与 Go 实现的差异

| 环节 | Go（`provider/workflow/comfyui.go`） | 本包 |
| --- | --- | --- |
| 提交 | `POST /prompt`，`client_id` 由宿主随机生成 16 字节 hex | `client_id` 取 `request.workflow.clientId`，缺省即省略；声明式表达式没有随机数算子 |
| 参考图 | 走受保护素材端口读本机字节 → multipart 表单字段 `type`/`overwrite` | `uploads[]` 用 `item.dataUrl` 与查询参数 `type`/`overwrite` |
| 回填 LoadImage | `buildComfyUIPromptBody` 按 `WorkflowFields` + `files[media.ID]` 写入节点 | **表达不了**，见下节 |
| 轮询 | `workflow.PollPolicy`（图片 1s 起、2s 间隔） | 声明 `poll.policy` 2s/1800s，当前宿主仍用内置 2.5s |
| 产物枚举 | 遍历 `outputs` 下所有节点 id，`images/gifs/videos/audio` 都收，节点号按数值序稳定排列 | 只能读声明的单个 `outputNodeId` |
| 失败信息 | 汇总所有 `status.messages`，用 `；` 连接，带节点号 | 取第一条，同样带节点号 |
| 取消 | `CancelComfyUI` 两步 + `QueryComfyUICancellation` 四态对账 | 两步声明；**取消对账没有声明端口** |
| 400 校验错误 | 解析 `error`/`node_errors` 拼成中文消息 | 由宿主统一处理 HTTP 400 与响应体，不在 manifest 内 |
| 类型校正 | 按 `/object_info` 把 INT/FLOAT/BOOLEAN 校正后再下发 | 图已由宿主映射完成，本包原样透传 |

## 宿主必须保留的接缝（表达不了的环节）

以下不是实现偷懒，而是当前契约表达不出来，必须留在宿主：

1. **多张参考图回填进图**。`uploads[].capture` 只把 `name`/`subfolder`/`type` 放进求值环境；把第 i 项写进第 i 个 `LoadImage` 节点的 `inputs.image`，需要「按运行时路径写回 `request.workflow.json`」的算子，而当前操作符全部是**只读取**，没有 `$set`/`$setPath`，也没有按 index 生成动态键写回嵌套对象的手段。ComfyUI 还要求 `image` 值写成 `subfolder/name`（subfolder 为空时就是 `name`），这层拼接同样只能在宿主映射时完成。因此预上传步骤与字段映射必须**交错**在宿主侧执行，manifest 只能声明「上传这件事的形状」。
2. **枚举 `outputs` 的全部节点**。`outputs` 的键是节点 id，没有「取 map 所有值」的算子（`$map`/`$filter` 的 `from` 遇到 map 只当单个元素处理，`$at`/`$first` 同样不展开 map）。所以只能读声明的单个 `outputNodeId`；多个 `SaveImage` 节点会漏产物，未声明时取不到结果。
3. **取消对账**。`cancel`/`cancelOperations` 只表达「发哪些请求」。Go 的 `QueryComfyUICancellation` 用 `GET /queue` + `GET /history` 判 `pending/confirmed/succeeded/failed` 四态，声明式协议没有取消查询端口。
4. **`/prompt` 400 的节点级错误文本**。`validations[].assert` 只在提交**前**求值且只看得见 `request`；`errorPaths`/`messagePaths` 是静态点分路径，无法定位 `{{taskId}}` 之下的响应。宿主若直接把 HTTP 400 正文交出去，会丢掉 Go 版本里「哪个节点的哪个输入不合法」这一层。
5. **随机 `client_id`**。无随机/UUID 算子，必须由宿主在 `request.workflow.clientId` 里给。
6. **数字键工作流图被误判成数组**。宿主会把请求体里的 map 递归规范化，若某份工作流的节点 id 恰为 `"0"`、`"1"`、`"2"`…连续序列，会被判成数组而破坏图结构。ComfyUI 导出的节点 id 通常从 `1` 开始，因此实际很少触发，但这是宿主行为，manifest 无法约束。

## 契约依赖状态

| 声明 | 状态 |
| --- | --- |
| `$urlencode`、`$lookup`、`$map`/`$filter`/`$first`/`$concat`/`$concatArrays`/`$merge`/`$if`/`$switch`/`$omitEmpty`/`$coalesce`/`$len`/`$at`/`$literal` | **现在就合法**，`internal/protocol/expression.go` 已实现 |
| `cancelOperations`、`originPath`、`validations[].assert`、`response.taskIdPaths` | **现在就合法**，`ManifestProvider`/`ManifestOperation` 已有对应 json tag |
| 响应环境 `baseUrl` | **现在就合法**，`manifestAdapter.parse` 已注入 |
| `$get` | 已落到 `expression.go`（`case "$get"`），本包 `status`/`message`/`images` 依赖它 |
| `providers[].uploads[]`（`foreach`/`capture`，环境键 `uploads.<id>`） | `ManifestUpload` 类型与 `newManifestUploadValues` 已在 `internal/protocol`，但**宿主执行链尚未消费**：`internal/generation` 里没有调用预上传的地方，图生图预上传目前不生效 |
| `poll.policy` | `ManifestOperation.Policy` 字段已存在，宿主仍按 `DeclarativeProtocolPollPolicy` 的内置 2.5s 间隔轮询，未读声明值 |
| `request.workflow.json` / `request.workflow.clientId` | `WorkflowPayload{json, clientId}` 与 `manifestRequestValues` 的 `workflow` 键已就位，但**还没有把映射好的图填入 `GenerationRequest.Workflow` 的写入点**，取到的是 nil |
| 多节点产物枚举、按 index 回填 `LoadImage`、取消对账 | 契约里**没有**对应键，需要新增操作符或新增端口声明，不能靠现有语法硬凑 |

也就是说：本包能过 JSON 与字段名校验，`$get` 也已可用，但真正跑通还差三处宿主接线（填充 `request.workflow`、执行 `uploads[]`、消费 `poll.policy`），以及上面列出的表达力缺口。切流必须排在最后。

## 兼容边界

只覆盖已在真机验收过的三段链路：文生图、图生图（参考图）、取消。不支持视频与音频参考、蒙版（`validations` 显式拒绝而不是静默丢弃素材）、WebSocket 进度、队列管理与 `/object_info` 拉取。上游要求六个根路径端点：`POST /prompt`、`GET /history/{prompt_id}`、`GET /view`、`POST /upload/image`、`POST /queue`、`POST /interrupt`。工作流必须是 API 格式而非 UI 导出。

<!-- BEEFTV_PLUGIN_MANIFEST_START -->
## Manifest 完整接口定义

以下 JSON 与插件包内实际 `manifest.json` 逐字段一致，覆盖插件身份、权限、配置、鉴权、参数、校验、创建、Agent、查询、取消、结果下载、响应和 Agent 响应映射。`documentation` 字段的值就是当前完整文档；为避免文档在自身内部无限递归，JSON 中仅用等义占位文本表示正文。

```json
{
  "apiVersion": "beeftv.plugin/v2",
  "id": "comfyui-local",
  "name": "本地 ComfyUI（原生协议）",
  "version": "2.0.0",
  "author": "BeefTV Contributors",
  "description": "本机 ComfyUI 原生 /prompt、/history、/view、/upload/image、/queue 与 /interrupt 协议的声明式插件。",
  "documentation": "<当前插件的完整 documentation，由 README.md 与 docs/interface.md 拼接而成；为避免 JSON 递归，此处不重复展开正文。>",
  "permissions": [
    "generation.run",
    "media.read"
  ],
  "configuration": {
    "fields": []
  },
  "contributes": {
    "providers": [
      {
        "id": "comfyui-workflow-image",
        "label": "本地 ComfyUI（原生协议）",
        "capabilities": [
          "image"
        ],
        "scopes": [
          "admin.system-channel",
          "user.custom-channel",
          "canvas",
          "creation"
        ],
        "baseUrl": "http://127.0.0.1:8188",
        "requiresPublicMediaUrls": false,
        "auth": {
          "type": "none"
        },
        "parameters": [
          {
            "name": "model",
            "type": "string",
            "required": true,
            "mapping": "不进请求体",
            "description": "工作流标识：宿主据此挑选要映射的 API 格式工作流。ComfyUI 的 /prompt 不接受模型名，模型本身写在图里的 CheckpointLoaderSimple 等节点上。"
          },
          {
            "name": "workflow",
            "type": "object",
            "required": true,
            "mapping": "create.body.prompt（取 request.workflow.json）",
            "description": "宿主按字段映射写好之后的完整 API 格式图对象，经 request.workflow.json 下发；同批的 request.workflow.clientId 作为可选的 create.body.client_id。本包原样交给 /prompt，不解析也不改写任何节点。"
          },
          {
            "name": "prompt",
            "type": "string",
            "mapping": "工作流内 CLIPTextEncode.text（宿主映射）",
            "description": "正向提示词。由宿主写入图内的文本编码节点，不出现在 /prompt 请求体顶层。"
          },
          {
            "name": "images",
            "type": "media[]",
            "mapping": "uploads[referenceImage] 与 工作流内 LoadImage.image（宿主回填）",
            "description": "图生图的参考图。先由 uploads[] 预上传到 ComfyUI 的 input 目录，文件名按输入顺序回填进图里的 LoadImage 节点；回填本身属于宿主映射。"
          },
          {
            "name": "resolution",
            "type": "string",
            "mapping": "工作流内 EmptyLatentImage.width/height（宿主映射）",
            "description": "输出分辨率档位或 WxH 尺寸，由宿主拆成宽高写入图内。"
          },
          {
            "name": "aspectRatio",
            "type": "string",
            "mapping": "工作流内 EmptyLatentImage.width/height（宿主映射）",
            "description": "画面比例，由宿主换算成宽高后写入图内。"
          },
          {
            "name": "imageCount",
            "type": "integer",
            "mapping": "工作流内 EmptyLatentImage.batch_size（宿主映射）",
            "description": "单次提交张数，映射为 latent 批大小。"
          },
          {
            "name": "steps",
            "type": "integer",
            "mapping": "工作流内 KSampler.steps（宿主映射）",
            "description": "采样步数。必须写成 JSON 数字：/prompt 会按 /object_info 声明的类型校验，INT 收到字符串会整张图被 400 拒绝。"
          },
          {
            "name": "samplerName",
            "type": "string",
            "mapping": "工作流内 KSampler.sampler_name（宿主映射）",
            "values": [
              "euler",
              "euler_a",
              "heun",
              "dpm_2",
              "dpm_2_a",
              "dpm_fast",
              "dpm_adaptive",
              "dpmpp_2s_a",
              "dpmpp_2m",
              "dpmpp_sde",
              "dpmpp_2m_sde",
              "dpmpp_3m_sde",
              "ddim",
              "uni_pc",
              "lms"
            ],
            "description": "采样器。可选值以该实例的 /object_info 为准，本表只是常见内置项。"
          },
          {
            "name": "scheduler",
            "type": "string",
            "mapping": "工作流内 KSampler.scheduler（宿主映射）",
            "values": [
              "simple",
              "karras",
              "exponential",
              "sgm_uniform",
              "normal",
              "ddpm_uniform"
            ],
            "description": "噪声调度器。与 samplerName 组合决定采样行为，同样以 /object_info 为准。"
          },
          {
            "name": "cfg",
            "type": "number",
            "mapping": "工作流内 KSampler.cfg（宿主映射）",
            "description": "提示词引导强度，FLOAT 类型，必须保持数字。"
          },
          {
            "name": "denoise",
            "type": "number",
            "mapping": "工作流内 KSampler.denoise（宿主映射）",
            "description": "重绘幅度，0~1 的 FLOAT。图生图关键参数：0 完全不改变参考图，1 完全重画。"
          },
          {
            "name": "seed",
            "type": "integer",
            "mapping": "工作流内 KSampler.seed（宿主映射）",
            "description": "随机种子。宿主随机取值上限是 2^53-1（JavaScript 安全整数），ComfyUI 声明的 uint64 上限不能直接用。"
          },
          {
            "name": "providerOptions",
            "type": "object",
            "mapping": "provider-specific fields",
            "description": "插件命名空间 comfyui-local 内的扩展字段：outputNodeId（读取产物用的节点 id）。client_id 不在这里，它随 request.workflow.clientId 由宿主下发。"
          }
        ],
        "validations": [
          {
            "assert": {
              "$gt": [
                {
                  "$len": {
                    "$ref": "request.workflow.json"
                  }
                },
                0
              ]
            },
            "message": "ComfyUI 缺少已映射的工作流图：请先在渠道里粘贴 API 格式工作流并配置字段映射"
          },
          {
            "assert": {
              "$or": [
                {
                  "$eq": [
                    {
                      "$ref": "request.capability"
                    },
                    "image"
                  ]
                },
                {
                  "$eq": [
                    {
                      "$ref": "request.capability"
                    },
                    ""
                  ]
                }
              ]
            },
            "message": "本地 ComfyUI 渠道只声明了 image 能力，请用图片生成模式提交"
          },
          {
            "assert": {
              "$and": [
                {
                  "$eq": [
                    {
                      "$len": {
                        "$ref": "request.videos"
                      }
                    },
                    0
                  ]
                },
                {
                  "$eq": [
                    {
                      "$len": {
                        "$ref": "request.audios"
                      }
                    },
                    0
                  ]
                },
                {
                  "$eq": [
                    {
                      "$len": {
                        "$filter": {
                          "from": {
                            "$ref": "request.images"
                          },
                          "as": "media",
                          "where": {
                            "$in": [
                              {
                                "$lower": {
                                  "$toString": {
                                    "$ref": "media.role"
                                  }
                                }
                              },
                              [
                                "mask",
                                "inpaint_mask",
                                "video",
                                "audio"
                              ]
                            ]
                          }
                        }
                      }
                    },
                    0
                  ]
                }
              ]
            },
            "message": "ComfyUI 暂不支持视频、音频参考素材与蒙版"
          }
        ],
        "uploads": [
          {
            "id": "referenceImage",
            "foreach": {
              "$ref": "request.images"
            },
            "operation": {
              "method": "POST",
              "path": "/upload/image",
              "contentType": "multipart/form-data",
              "originPath": true,
              "files": [
                {
                  "name": "image",
                  "source": {
                    "$ref": "item.dataUrl"
                  },
                  "filename": {
                    "$ref": "item.name"
                  }
                }
              ],
              "query": {
                "type": {
                  "$literal": "input"
                },
                "overwrite": {
                  "$literal": "true"
                }
              }
            },
            "capture": {
              "name": {
                "$ref": "response.name"
              },
              "subfolder": {
                "$ref": "response.subfolder"
              },
              "type": {
                "$ref": "response.type"
              }
            }
          }
        ],
        "create": {
          "method": "POST",
          "path": "/prompt",
          "contentType": "application/json",
          "originPath": true,
          "body": {
            "prompt": {
              "$ref": "request.workflow.json"
            },
            "client_id": {
              "$omitEmpty": {
                "$ref": "request.workflow.clientId"
              }
            }
          }
        },
        "poll": {
          "method": "GET",
          "path": "/history/{{taskId}}",
          "contentType": "application/json",
          "originPath": true,
          "policy": {
            "intervalSeconds": 2,
            "timeoutSeconds": 1800
          }
        },
        "cancel": {
          "method": "POST",
          "path": "/queue",
          "contentType": "application/json",
          "originPath": true,
          "body": {
            "delete": [
              {
                "$ref": "taskId"
              }
            ]
          }
        },
        "cancelOperations": [
          {
            "method": "POST",
            "path": "/interrupt",
            "contentType": "application/json",
            "originPath": true,
            "body": {
              "prompt_id": {
                "$ref": "taskId"
              }
            }
          }
        ],
        "response": {
          "taskIdPaths": [
            "prompt_id"
          ],
          "resultKind": "image",
          "resultEphemeral": true,
          "status": {
            "$switch": {
              "cases": [
                {
                  "when": {
                    "$eq": [
                      {
                        "$len": {
                          "$get": [
                            {
                              "$ref": "response"
                            },
                            {
                              "$ref": "taskId"
                            }
                          ]
                        }
                      },
                      0
                    ]
                  },
                  "then": "queued"
                },
                {
                  "when": {
                    "$gt": [
                      {
                        "$len": {
                          "$filter": {
                            "from": {
                              "$get": [
                                {
                                  "$get": [
                                    {
                                      "$get": [
                                        {
                                          "$ref": "response"
                                        },
                                        {
                                          "$ref": "taskId"
                                        }
                                      ]
                                    },
                                    {
                                      "$literal": "status"
                                    }
                                  ]
                                },
                                {
                                  "$literal": "messages"
                                }
                              ]
                            },
                            "as": "message",
                            "where": {
                              "$eq": [
                                {
                                  "$lower": {
                                    "$toString": {
                                      "$ref": "message.0"
                                    }
                                  }
                                },
                                "execution_interrupted"
                              ]
                            }
                          }
                        }
                      },
                      0
                    ]
                  },
                  "then": "cancelled"
                },
                {
                  "when": {
                    "$eq": [
                      {
                        "$lower": {
                          "$trim": {
                            "$toString": {
                              "$get": [
                                {
                                  "$get": [
                                    {
                                      "$get": [
                                        {
                                          "$ref": "response"
                                        },
                                        {
                                          "$ref": "taskId"
                                        }
                                      ]
                                    },
                                    {
                                      "$literal": "status"
                                    }
                                  ]
                                },
                                {
                                  "$literal": "status_str"
                                }
                              ]
                            }
                          }
                        }
                      },
                      "error"
                    ]
                  },
                  "then": "failed"
                },
                {
                  "when": {
                    "$toBool": {
                      "$get": [
                        {
                          "$get": [
                            {
                              "$get": [
                                {
                                  "$ref": "response"
                                },
                                {
                                  "$ref": "taskId"
                                }
                              ]
                            },
                            {
                              "$literal": "status"
                            }
                          ]
                        },
                        {
                          "$literal": "completed"
                        }
                      ]
                    }
                  },
                  "then": "succeeded"
                },
                {
                  "when": {
                    "$gt": [
                      {
                        "$len": {
                          "$get": [
                            {
                              "$get": [
                                {
                                  "$get": [
                                    {
                                      "$ref": "response"
                                    },
                                    {
                                      "$ref": "taskId"
                                    }
                                  ]
                                },
                                {
                                  "$literal": "outputs"
                                }
                              ]
                            },
                            {
                              "$ref": "request.providerOptions.comfyui-local.outputNodeId"
                            }
                          ]
                        }
                      },
                      0
                    ]
                  },
                  "then": "succeeded"
                }
              ],
              "default": "executing"
            }
          },
          "message": {
            "$omitEmpty": {
              "$first": {
                "$map": {
                  "from": {
                    "$filter": {
                      "from": {
                        "$get": [
                          {
                            "$get": [
                              {
                                "$get": [
                                  {
                                    "$ref": "response"
                                  },
                                  {
                                    "$ref": "taskId"
                                  }
                                ]
                              },
                              {
                                "$literal": "status"
                              }
                            ]
                          },
                          {
                            "$literal": "messages"
                          }
                        ]
                      },
                      "as": "message",
                      "where": {
                        "$in": [
                          {
                            "$lower": {
                              "$toString": {
                                "$ref": "message.0"
                              }
                            }
                          },
                          [
                            "execution_error",
                            "execution_interrupted"
                          ]
                        ]
                      }
                    }
                  },
                  "as": "message",
                  "in": {
                    "$concat": [
                      {
                        "$if": {
                          "condition": {
                            "$ne": [
                              {
                                "$get": [
                                  {
                                    "$ref": "message.1"
                                  },
                                  {
                                    "$literal": "node_id"
                                  }
                                ]
                              },
                              null
                            ]
                          },
                          "then": {
                            "$concat": [
                              "节点 ",
                              {
                                "$get": [
                                  {
                                    "$ref": "message.1"
                                  },
                                  {
                                    "$literal": "node_id"
                                  }
                                ]
                              },
                              "："
                            ]
                          },
                          "else": null
                        }
                      },
                      {
                        "$coalesce": [
                          {
                            "$get": [
                              {
                                "$ref": "message.1"
                              },
                              {
                                "$literal": "exception_message"
                              }
                            ]
                          },
                          {
                            "$get": [
                              {
                                "$ref": "message.1"
                              },
                              {
                                "$literal": "exception_type"
                              }
                            ]
                          },
                          {
                            "$if": {
                              "condition": {
                                "$eq": [
                                  {
                                    "$lower": {
                                      "$toString": {
                                        "$ref": "message.0"
                                      }
                                    }
                                  },
                                  "execution_interrupted"
                                ]
                              },
                              "then": "任务已被中断",
                              "else": null
                            }
                          },
                          {
                            "$literal": "ComfyUI 执行失败"
                          }
                        ]
                      }
                    ]
                  }
                }
              }
            }
          },
          "images": {
            "$map": {
              "from": {
                "$concatArrays": [
                  {
                    "$get": [
                      {
                        "$get": [
                          {
                            "$get": [
                              {
                                "$get": [
                                  {
                                    "$ref": "response"
                                  },
                                  {
                                    "$ref": "taskId"
                                  }
                                ]
                              },
                              {
                                "$literal": "outputs"
                              }
                            ]
                          },
                          {
                            "$ref": "request.providerOptions.comfyui-local.outputNodeId"
                          }
                        ]
                      },
                      {
                        "$literal": "images"
                      }
                    ]
                  },
                  {
                    "$get": [
                      {
                        "$get": [
                          {
                            "$get": [
                              {
                                "$get": [
                                  {
                                    "$ref": "response"
                                  },
                                  {
                                    "$ref": "taskId"
                                  }
                                ]
                              },
                              {
                                "$literal": "outputs"
                              }
                            ]
                          },
                          {
                            "$ref": "request.providerOptions.comfyui-local.outputNodeId"
                          }
                        ]
                      },
                      {
                        "$literal": "gifs"
                      }
                    ]
                  },
                  {
                    "$get": [
                      {
                        "$get": [
                          {
                            "$get": [
                              {
                                "$get": [
                                  {
                                    "$ref": "response"
                                  },
                                  {
                                    "$ref": "taskId"
                                  }
                                ]
                              },
                              {
                                "$literal": "outputs"
                              }
                            ]
                          },
                          {
                            "$ref": "request.providerOptions.comfyui-local.outputNodeId"
                          }
                        ]
                      },
                      {
                        "$literal": "videos"
                      }
                    ]
                  },
                  {
                    "$get": [
                      {
                        "$get": [
                          {
                            "$get": [
                              {
                                "$get": [
                                  {
                                    "$ref": "response"
                                  },
                                  {
                                    "$ref": "taskId"
                                  }
                                ]
                              },
                              {
                                "$literal": "outputs"
                              }
                            ]
                          },
                          {
                            "$ref": "request.providerOptions.comfyui-local.outputNodeId"
                          }
                        ]
                      },
                      {
                        "$literal": "audio"
                      }
                    ]
                  }
                ]
              },
              "as": "file",
              "in": {
                "$if": {
                  "condition": {
                    "$ne": [
                      {
                        "$ref": "file.filename"
                      },
                      null
                    ]
                  },
                  "then": {
                    "url": {
                      "$concat": [
                        {
                          "$ref": "baseUrl"
                        },
                        "/view?filename=",
                        {
                          "$urlencode": {
                            "$ref": "file.filename"
                          }
                        },
                        "&subfolder=",
                        {
                          "$urlencode": {
                            "$ref": "file.subfolder"
                          }
                        },
                        "&type=",
                        {
                          "$urlencode": {
                            "$coalesce": [
                              {
                                "$ref": "file.type"
                              },
                              {
                                "$literal": "output"
                              }
                            ]
                          }
                        }
                      ]
                    },
                    "name": {
                      "$omitEmpty": {
                        "$ref": "file.filename"
                      }
                    },
                    "kind": "image"
                  },
                  "else": null
                }
              }
            }
          }
        }
      }
    ]
  }
}
```
<!-- BEEFTV_PLUGIN_MANIFEST_END -->
