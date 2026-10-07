package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"infinite-canvas/backend/internal/model"
	"infinite-canvas/backend/internal/protocol"
	"infinite-canvas/backend/internal/provider/workflow"
	"infinite-canvas/backend/internal/repository"
)

const (
	providerCancellationLeaseDuration = time.Minute
	providerCancellationMaxAttempts   = 41
)

type providerCancellationOutcome string

type geminiOperation map[string]any

const (
	providerCancellationPending   providerCancellationOutcome = "pending"
	providerCancellationConfirmed providerCancellationOutcome = "confirmed"
	providerCancellationSucceeded providerCancellationOutcome = "succeeded"
	providerCancellationFailed    providerCancellationOutcome = "failed"
)

func (s *Service) startProviderCancellationReconciliation(ctx context.Context) {
	s.runWorkerLoop(func(ctx context.Context) {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				task, err := s.repo.ClaimNextTaskProviderCancellation("provider-cancel:"+s.workerID, providerCancellationLeaseDuration)
				if err != nil || task == nil {
					continue
				}
				reconcileCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				if err := s.reconcileProviderCancellation(reconcileCtx, task); err != nil {
					_ = s.log(task.UserID, task.ID, "error", "上游取消状态对账失败", err.Error())
				}
				cancel()
			}
		}
	})
}

func (s *Service) requestProviderCancellation(ctx context.Context, task *model.Task) error {
	if task == nil || task.ID == "" {
		return errors.New("任务取消状态无效")
	}
	// 取消与对账由独立 worker loop 触发，ctx 不携带 create 路径注入的插件注册表。
	// 声明式 cancel 操作必须先从注册表解析，否则已声明取消的插件渠道会被判为不支持。
	ctx = withProtocolRegistry(ctx, s.protocolRegistry())
	s.hydrateTaskProviderRequestID(task)
	if task.ProviderRequestID != "" {
		if err := s.repo.UpdateTaskProviderState(task.ID, task.ProviderRequestID, task.PollStage, task.NextPollAt); err != nil {
			return err
		}
	}
	if err := s.repo.ClaimTaskProviderCancellation(task.UserID, task.ID, time.Now()); err != nil {
		if errors.Is(err, repository.ErrTaskProviderCancellationConflict) {
			return nil
		}
		return err
	}
	if task.ProviderRequestID == "" {
		return s.markProviderCancellationUncertain(task, providerCancellationUncertainMessage("上游未返回任务 ID，无法发送取消请求"))
	}

	input, err := s.providerCancellationInput(task)
	if err != nil {
		return s.markProviderCancellationUncertain(task, providerCancellationUncertainMessage("读取上游取消配置失败")+"："+err.Error())
	}
	if !supportsProviderCancellation(ctx, input.Config, task.ProviderRequestID) {
		return s.markProviderCancellationUncertain(task, providerCancellationUncertainMessage("当前上游协议不支持取消"))
	}

	requestCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	requestTask := *task
	requestTask.InputJSON = mustJSON(input)
	requestCtx = withProviderRequestKind(withProviderAnalytics(requestCtx, s, requestTask), "cancel")
	if err := cancelProviderTask(requestCtx, input.Config, task.ProviderRequestID); err != nil {
		return s.markProviderCancellationUncertain(task, providerCancellationUncertainMessage("上游取消请求结果不明确")+"："+safeProviderLogError(err))
	}
	_ = s.log(task.UserID, task.ID, "info", "已请求上游取消，等待供应商确认", task.ProviderRequestID)
	return nil
}

func (s *Service) reconcileProviderCancellation(ctx context.Context, task *model.Task) error {
	ctx = withProtocolRegistry(ctx, s.protocolRegistry())
	if task.ProviderCancelAttempts >= providerCancellationMaxAttempts {
		return s.markProviderCancellationUncertain(task, providerCancellationUncertainMessage("上游取消状态长时间未确认"))
	}
	input, err := s.providerCancellationInput(task)
	if err != nil {
		return s.markProviderCancellationUncertain(task, providerCancellationUncertainMessage("读取上游对账配置失败")+"："+err.Error())
	}
	queryTask := *task
	queryTask.InputJSON = mustJSON(input)
	queryCtx := withProviderRequestKind(withProviderAnalytics(ctx, s, queryTask), "cancel-query")
	outcome, providerStatus, err := queryProviderCancellation(queryCtx, input.Config, task.ProviderRequestID)
	if err != nil {
		if task.ProviderCancelAttempts >= providerCancellationMaxAttempts-1 {
			return s.markProviderCancellationUncertain(task, providerCancellationUncertainMessage("连续查询上游取消状态失败")+"："+safeProviderLogError(err))
		}
		next := time.Now().Add(30 * time.Second)
		return s.repo.UpdateTaskProviderCancellation(task.ID, model.ProviderCancelStatusRequested, model.ProviderCancelStatusRequested, safeProviderLogError(err), &next, nil)
	}

	switch outcome {
	case providerCancellationConfirmed:
		now := time.Now()
		if err := s.repo.UpdateTaskProviderCancellation(task.ID, model.ProviderCancelStatusRequested, model.ProviderCancelStatusConfirmed, "", nil, &now); err != nil {
			return err
		}
		_ = s.log(task.UserID, task.ID, "info", "上游已确认取消", providerStatus)
	case providerCancellationSucceeded:
		errorText := "取消请求发出前上游已完成"
		if err := s.repo.UpdateTaskProviderCancellation(task.ID, model.ProviderCancelStatusRequested, model.ProviderCancelStatusUncertain, errorText, nil, nil); err != nil {
			return err
		}
		_ = s.log(task.UserID, task.ID, "warn", "上游任务在取消确认前已完成", providerStatus)
	case providerCancellationFailed:
		errorText := "上游任务已失败，取消状态无法确认"
		if err := s.repo.UpdateTaskProviderCancellation(task.ID, model.ProviderCancelStatusRequested, model.ProviderCancelStatusUncertain, errorText, nil, nil); err != nil {
			return err
		}
		_ = s.log(task.UserID, task.ID, "warn", errorText, providerStatus)
	default:
		next := time.Now().Add(15 * time.Second)
		return s.repo.UpdateTaskProviderCancellation(task.ID, model.ProviderCancelStatusRequested, model.ProviderCancelStatusRequested, "", &next, nil)
	}
	return nil
}

func providerCancellationUncertainMessage(base string) string {
	return base + "，取消状态待确认"
}

func (s *Service) deferProviderCancellation(task *model.Task, errorText string) error {
	next := time.Now().Add(30 * time.Second)
	return s.repo.UpdateTaskProviderCancellation(task.ID, model.ProviderCancelStatusRequested, model.ProviderCancelStatusRequested, truncateRunes(errorText, 1000), &next, nil)
}

func (s *Service) markProviderCancellationUncertain(task *model.Task, errorText string) error {
	message := truncateRunes(errorText, 1000)
	if err := s.repo.UpdateTaskProviderCancellation(task.ID, model.ProviderCancelStatusRequested, model.ProviderCancelStatusUncertain, message, nil, nil); err != nil {
		return err
	}
	_ = s.log(task.UserID, task.ID, "warn", "上游取消无法确认", message)
	return nil
}

func (s *Service) providerCancellationInput(task *model.Task) (canvasGenerationInput, error) {
	decrypted, err := s.decryptTaskInputJSON(task.InputJSON)
	if err != nil {
		return canvasGenerationInput{}, err
	}
	var input canvasGenerationInput
	if err := json.Unmarshal([]byte(decrypted), &input); err != nil {
		return canvasGenerationInput{}, err
	}
	config, err := s.resolveProviderConfig(input.Config)
	if err != nil {
		return canvasGenerationInput{}, err
	}
	input.Config = config
	return input, nil
}

// supportsProviderCancellation 先认宿主已实现的厂商取消，再认插件声明：
// 只有当前 ctx 能解析出声明式适配器、manifest 声明了 cancel 操作，并且 BuildCancel
// 针对这条真实上游任务产出可执行 RequestSpec 时，才判定该接口可取消。
// 判定失败必须保持显式不支持，不能退化成静默空操作。
func supportsProviderCancellation(ctx context.Context, config providerConfig, providerRequestID string) bool {
	if config.InterfaceType == string(model.ChannelInterfaceGeminiVeo) ||
		model.IsVolcengineArkVideoProtocol(model.ChannelInterfaceType(config.InterfaceType)) ||
		isComfyUIInterface(config.InterfaceType) {
		return true
	}
	_, ok := declarativeCancelSpec(ctx, config, providerRequestID)
	return ok
}

// declarativeCancelSpec 读取插件声明的 cancel 操作。manifest 只描述 method/path/body，
// 凭证注入、SSRF 校验、超时和回执仍由 executeProtocolRequest 的宿主出站边界统一执行。
func declarativeCancelSpec(ctx context.Context, config providerConfig, providerRequestID string) (protocol.RequestSpec, bool) {
	adapter, ok := declarativeProtocolAdapterForContext(ctx, config.InterfaceType)
	if !ok || strings.TrimSpace(adapter.Metadata().Cancel) == "" {
		return protocol.RequestSpec{}, false
	}
	spec, err := adapter.BuildCancel(ctx, declarativeCancellationPollContext(config, providerRequestID))
	if err != nil || spec.Validate() != nil {
		return protocol.RequestSpec{}, false
	}
	return spec, true
}

// 声明式取消只依赖任务身份：已安装插件的 cancel 操作都以 {{taskId}} 定位上游任务，
// 模型与 Base URL 取自渠道配置，不回读原始生成请求，也不写入任何凭证明文。
func declarativeCancellationPollContext(config providerConfig, providerRequestID string) protocol.PollContext {
	return protocol.PollContext{
		BaseURL: config.BaseURL,
		Model:   config.Model,
		Request: protocol.GenerationRequest{Model: config.Model},
		TaskID:  providerRequestID,
	}
}

func cancelProviderTask(ctx context.Context, config providerConfig, providerRequestID string) error {
	switch {
	case config.InterfaceType == string(model.ChannelInterfaceGeminiVeo):
		path := "/" + strings.TrimLeft(providerRequestID, "/") + ":cancel"
		return postGeminiJSON(ctx, config, path, map[string]any{}, &map[string]any{})
	case model.IsVolcengineArkVideoProtocol(model.ChannelInterfaceType(config.InterfaceType)):
		path := "/contents/generations/tasks/" + url.PathEscape(providerRequestID)
		return deleteProviderTask(ctx, config, path)
	case isComfyUIInterface(config.InterfaceType):
		return cancelComfyUITask(ctx, config, providerRequestID)
	default:
		spec, ok := declarativeCancelSpec(ctx, config, providerRequestID)
		if !ok {
			return errors.New("当前上游协议不支持取消")
		}
		_, err := executeProtocolRequest(bindRequestReceipts(ctx), config, spec)
		return err
	}
}

// comfyUIWorkflowClient 只用到出站执行端口：ComfyUI 的取消与对账不落本地回执。
func comfyUIWorkflowClient() *workflow.Client {
	return &workflow.Client{Requests: workflowRequestExecutor{}}
}

func cancelComfyUITask(ctx context.Context, config providerConfig, promptID string) error {
	return comfyUIWorkflowClient().CancelComfyUI(ctx, workflowConfigFromProvider(config), promptID)
}

func queryComfyUICancellation(ctx context.Context, config providerConfig, promptID string) (providerCancellationOutcome, string, error) {
	result, err := comfyUIWorkflowClient().QueryComfyUICancellation(ctx, workflowConfigFromProvider(config), promptID)
	if err != nil {
		return "", "", err
	}
	return providerCancellationOutcome(result.Outcome), result.Status, nil
}

func deleteProviderTask(ctx context.Context, config providerConfig, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, apiURL(config.BaseURL, path), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+config.APIKey)
	ApplyOutboundHeaders(req, config.Headers)
	data, _, err := doBinary(req)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) > 0 && !json.Valid(data) {
		return errors.New("上游取消接口返回了无法识别的内容")
	}
	return nil
}

func queryProviderCancellation(ctx context.Context, config providerConfig, providerRequestID string) (providerCancellationOutcome, string, error) {
	switch {
	case config.InterfaceType == string(model.ChannelInterfaceGeminiVeo):
		// Gemini 用 operation.error 表示取消终态，此处必须保留该字段再判断，
		// 不能让通用 JSON 解包提前把它转换成请求失败。
		var operation geminiOperation
		if err := getGeminiJSON(ctx, config, "/"+strings.TrimLeft(providerRequestID, "/"), &operation); err != nil {
			return "", "", err
		}
		if done, _ := operation["done"].(bool); !done {
			return providerCancellationPending, "running", nil
		}
		if errorValue, ok := operation["error"].(map[string]any); ok {
			code := strings.TrimSpace(fmt.Sprint(errorValue["code"]))
			message := strings.ToLower(stringField(errorValue, "message"))
			if code == "1" || code == "1.0" || strings.Contains(message, "cancel") {
				return providerCancellationConfirmed, "cancelled", nil
			}
			return providerCancellationFailed, firstNonEmpty(message, "failed"), nil
		}
		return providerCancellationSucceeded, "succeeded", nil
	case model.IsVolcengineArkVideoProtocol(model.ChannelInterfaceType(config.InterfaceType)):
		var state map[string]any
		if err := getJSON(ctx, config, "/contents/generations/tasks/"+url.PathEscape(providerRequestID), &state); err != nil {
			return "", "", err
		}
		if data, ok := state["data"].(map[string]any); ok {
			state = data
		}
		status := strings.ToLower(strings.TrimSpace(stringField(state, "status")))
		switch status {
		case "cancelled", "canceled":
			return providerCancellationConfirmed, status, nil
		case "succeeded", "completed":
			return providerCancellationSucceeded, status, nil
		case "failed", "expired":
			return providerCancellationFailed, status, nil
		case "queued", "running", "processing", "pending":
			return providerCancellationPending, status, nil
		default:
			return providerCancellationPending, firstNonEmpty(status, "unknown"), nil
		}
	case isComfyUIInterface(config.InterfaceType):
		return queryComfyUICancellation(ctx, config, providerRequestID)
	default:
		return queryDeclarativeCancellation(ctx, config, providerRequestID)
	}
}

// queryDeclarativeCancellation 只在插件同时声明状态查询（poll 操作）时对账取消结果。
// 没有声明状态查询时必须返回 pending/unknown，由上层租约继续重试直至超时；
// 绝不能把“无从确认”写成取消成功，也不能伪造供应商状态。
func queryDeclarativeCancellation(ctx context.Context, config providerConfig, providerRequestID string) (providerCancellationOutcome, string, error) {
	adapter, ok := declarativeProtocolAdapterForContext(ctx, config.InterfaceType)
	if !ok {
		return "", "", errors.New("当前上游协议不支持取消状态查询")
	}
	if strings.TrimSpace(adapter.Metadata().Poll) == "" {
		return providerCancellationPending, "unknown", nil
	}
	pollContext := declarativeCancellationPollContext(config, providerRequestID)
	spec, err := adapter.BuildPoll(ctx, pollContext)
	if err != nil || spec.Validate() != nil {
		return providerCancellationPending, "unknown", nil
	}
	body, err := executeProtocolRequest(bindRequestReceipts(ctx), config, spec)
	if err != nil {
		return "", "", err
	}
	state, err := adapter.ParsePoll(ctx, pollContext, body)
	if err != nil {
		return "", "", err
	}
	switch state.Status {
	case protocol.StatusCancelled:
		return providerCancellationConfirmed, string(state.Status), nil
	case protocol.StatusSucceeded:
		return providerCancellationSucceeded, string(state.Status), nil
	case protocol.StatusFailed:
		return providerCancellationFailed, string(state.Status), nil
	default:
		return providerCancellationPending, firstNonEmpty(string(state.Status), "unknown"), nil
	}
}

func mustJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}
