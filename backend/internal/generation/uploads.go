package generation

// 声明式预上传的宿主执行侧。manifest 的 uploads[] 由协议层展开成待执行步骤，宿主只做两件事：
// 逐个把步骤交给既有出站边界执行，把响应 JSON 解码成 Values 交回 ApplyUploadResults。
// 捕获映射、$ref 解释和 uploads 环境键的构造都留在协议层，宿主看不见任何表达式。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"infinite-canvas/backend/internal/protocol"
)

// protocolUploadRequestKind 复用 Ark 私资与 full-video 两条既有预上传的请求类别，
// 让回执与日志能把上传步骤和真正计费的 create 区分开。
const protocolUploadRequestKind = "media_upload"

type protocolUploadsContextKey struct{}

// withProtocolUploads 把本轮捕获到的 uploads 值挂在执行上下文上。BuildResult 在
// FinishProtocolAdapterResult 内部构造，而那个签名是 app 侧恢复路径的既有入口，不为预上传加参数；
// 取不到捕获值的上下文（恢复、取消）按“本轮没有 uploads”处理，表达式自然解析为空。
func withProtocolUploads(ctx context.Context, uploads map[string]any) context.Context {
	if len(uploads) == 0 {
		return ctx
	}
	return context.WithValue(ctx, protocolUploadsContextKey{}, uploads)
}

func protocolUploadsFromContext(ctx context.Context) map[string]any {
	uploads, _ := ctx.Value(protocolUploadsContextKey{}).(map[string]any)
	return uploads
}

// protocolRequestContext / protocolPollContext 是声明式适配器的统一入参形状：
// 除了 Base URL、请求和任务号，还要把执行上下文里的捕获值投影给协议层。
func protocolRequestContext(ctx context.Context, input Input, request protocol.GenerationRequest) protocol.RequestContext {
	return protocol.RequestContext{BaseURL: input.Config.BaseURL, Request: request, Uploads: protocolUploadsFromContext(ctx)}
}

func protocolPollContext(ctx context.Context, input Input, request protocol.GenerationRequest, taskID string) protocol.PollContext {
	return protocol.PollContext{BaseURL: input.Config.BaseURL, Model: request.Model, Request: request, TaskID: taskID, Uploads: protocolUploadsFromContext(ctx)}
}

// runProtocolUploads 在 create 之前执行 provider 声明的 uploads[]，返回的执行上下文带上捕获值。
// 未实现 UploadAdapter、未声明 uploads 或声明展开为空时一次 HTTP 都不发；恢复路径已有上游任务，
// 重复上传会造出第二份素材，因此整段跳过，与 Seedance 预上传对 resumed 的处理保持一致。
//
// 任一步失败都在 create 之前返回真实错误：生成写路径不允许“没上传成功也提交任务”。
func runProtocolUploads(ctx context.Context, input Input, adapter protocol.Adapter, request protocol.GenerationRequest) (context.Context, error) {
	if ResumedProviderRequestID(ctx) != "" {
		return ctx, nil
	}
	uploader, ok := adapter.(protocol.UploadAdapter)
	if !ok {
		return ctx, nil
	}
	steps, err := uploader.BuildUploadRequests(ctx, protocol.UploadContext{BaseURL: input.Config.BaseURL, Request: request})
	if err != nil {
		return ctx, err
	}
	if len(steps) == 0 {
		return ctx, nil
	}
	runtime, _ := RuntimeFromContext(ctx)
	if err := setExecutionStage(ctx, runtime, "正在上传参考素材"); err != nil {
		return ctx, err
	}
	results := make([]protocol.ManifestUploadResult, 0, len(steps))
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return ctx, err
		}
		body, err := ExecuteProtocolRequest(WithRequestKind(ctx, protocolUploadRequestKind), input.Config, step.Request)
		if err != nil {
			return ctx, fmt.Errorf("预上传 %s 第 %d 步失败：%w", step.ID, step.Index, err)
		}
		values, err := decodeProtocolUploadResponse(body)
		if err != nil {
			return ctx, fmt.Errorf("预上传 %s 第 %d 步失败：%w", step.ID, step.Index, err)
		}
		results = append(results, protocol.ManifestUploadResult{ID: step.ID, Index: step.Index, Values: values})
	}
	applied, err := uploader.ApplyUploadResults(ctx, protocolRequestContext(ctx, input, request), results)
	if err != nil {
		return ctx, fmt.Errorf("预上传结果无法用于 %s 协议：%w", adapter.Metadata().ID, err)
	}
	return withProtocolUploads(ctx, applied.Uploads), nil
}

// decodeProtocolUploadResponse 只把响应正文还原成 JSON 对象，不挑字段也不判断业务成败：
// 哪个键进 uploads.<id>.<字段> 完全由 manifest 的 capture 决定。空正文视作空对象，
// 让无正文的上传端点仍可搭配只引用 item/request 的捕获；非对象正文是真实不一致，必须报错。
func decodeProtocolUploadResponse(body []byte) (map[string]any, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return map[string]any{}, nil
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(trimmed), &values); err != nil {
		return nil, fmt.Errorf("上传响应不是 JSON 对象：%w", err)
	}
	return values, nil
}
