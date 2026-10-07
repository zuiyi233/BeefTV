package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"

	"infinite-canvas/backend/internal/model"
)

// providerRequestIDKeys 是"上游任务标识"候选键，按具体到泛化排序：取消、对账和人工恢复
// 只能拿这些值去定位上游那条任务，所以显式的任务句柄必须先于泛化的 id 命中。
//   - prompt_id：ComfyUI 原生 /prompt 的受理响应，同时也是 /history/{prompt_id}、
//     /queue delete、/interrupt 唯一的任务句柄（见 provider/workflow/comfyui.go）。
//   - taskId：声明式插件 catalog 里最常见的驼峰写法，与宿主 {{taskId}} 占位符同名。
//   - video_id：Agnes 视频协议把任务句柄放在 video_id，轮询即 query video_id={{taskId}}。
//
// 顺序上把 task_id/taskId/prompt_id/video_id 放在 id 之前，与既有实现"先认显式任务字段"
// 一致；插件自己声明的 taskIdPaths 优先由受理回执落库，这里只兜住回灌链路。
var providerRequestIDKeys = []string{"task_id", "taskId", "prompt_id", "video_id", "id", "request_id", "name"}

// EnrichAPICallLog extracts provider recovery identifiers, usage and failure
// details. This is operational task state, not SaaS engagement analytics.
func (s *Service) EnrichAPICallLog(log *model.ApiCallLog, responseBody []byte) {
	if log == nil {
		return
	}
	if log.ProviderRequestID == "" {
		log.ProviderRequestID = providerRequestIDFromPath(log.Path)
	}
	for _, payload := range providerResponsePayloads(responseBody) {
		s.enrichAPICallLogPayload(log, payload)
	}
	s.enrichAPICallLogFailureSummary(log, responseBody)
}

func (s *Service) enrichAPICallLogFailureSummary(log *model.ApiCallLog, responseBody []byte) {
	if log.Status != model.ApiCallStatusFailed {
		return
	}
	failure := classifyProviderHTTP(providerHTTPError{StatusCode: log.StatusCode, Body: string(responseBody)})
	// Persist only normalized diagnostics; provider messages may echo credentials or input.
	log.ErrorCode = firstNonEmpty(failure.ProviderCode, failure.ErrorCode())
	log.Error = failure.UserMessage()
}

func (s *Service) enrichAPICallLogPayload(log *model.ApiCallLog, payload map[string]any) {
	nestedTaskID := ""
	if data, ok := payload["data"].(map[string]any); ok {
		if log.Capability == "video" && strings.Contains(log.Path, "/v1/video/generations") {
			if extracted, err := firstJSONString(data, "task_id", "taskId"); err == nil {
				nestedTaskID = extracted
			}
		}
		for key, value := range data {
			if _, exists := payload[key]; !exists {
				payload[key] = value
			}
		}
	}
	if response, ok := payload["response"].(map[string]any); ok {
		for key, value := range response {
			if _, exists := payload[key]; !exists {
				payload[key] = value
			}
		}
	}
	if log.Status == model.ApiCallStatusFailed {
		errorCode, errorMessage := providerFailureDetails(payload)
		log.ErrorCode = errorCode
		if errorMessage != "" {
			log.Error = errorMessage
		}
	}
	if usage, ok := payload["usage"].(map[string]any); ok {
		inputTokens, inputAvailable := firstInt64Value(usage, "input_tokens", "prompt_tokens")
		outputTokens, outputAvailable := firstInt64Value(usage, "output_tokens", "completion_tokens")
		if inputAvailable {
			log.InputTokens = inputTokens
		}
		if outputAvailable {
			log.OutputTokens = outputTokens
		}
		if inputAvailable || outputAvailable {
			log.UsageAvailable = true
		}
		if log.Capability == "video" && strings.Contains(log.Path, "/contents/generations/tasks") {
			if log.OutputTokens == 0 {
				log.OutputTokens = firstInt64(usage, "total_tokens")
			}
			log.UsageAvailable = log.OutputTokens > 0
		}
		if details, ok := usage["input_tokens_details"].(map[string]any); ok {
			log.CachedTokens = firstInt64(details, "cached_tokens", "cache_read_input_tokens")
		}
		if details, ok := usage["prompt_tokens_details"].(map[string]any); ok && log.CachedTokens == 0 {
			log.CachedTokens = firstInt64(details, "cached_tokens", "cache_read_input_tokens")
		}
		if log.CachedTokens == 0 {
			log.CachedTokens = firstInt64(usage, "cached_tokens", "cache_read_input_tokens", "prompt_cache_hit_tokens")
		}
	}
	if usage, ok := payload["usageMetadata"].(map[string]any); ok {
		inputTokens, inputAvailable := firstInt64Value(usage, "promptTokenCount")
		outputTokens, outputAvailable := firstInt64Value(usage, "candidatesTokenCount")
		if inputAvailable {
			log.InputTokens = inputTokens
		}
		if outputAvailable {
			log.OutputTokens = outputTokens
		}
		if inputAvailable || outputAvailable {
			log.UsageAvailable = true
		}
		log.CachedTokens = firstInt64(usage, "cachedContentTokenCount")
	}
	if extracted, err := firstJSONString(payload, providerRequestIDKeys...); err == nil {
		log.ProviderRequestID = firstNonEmpty(nestedTaskID, extracted, log.ProviderRequestID)
	} else {
		log.ProviderRequestID = firstNonEmpty(nestedTaskID, log.ProviderRequestID)
	}
	log.ProviderStatus = strings.ToLower(firstNonEmpty(stringField(payload, "status"), log.ProviderStatus))
	if log.ProviderStatus == "failed" || log.ProviderStatus == "cancelled" || log.ProviderStatus == "expired" {
		log.Status = model.ApiCallStatusFailed
		errorCode, errorMessage := providerFailureDetails(payload)
		log.ErrorCode = firstNonEmpty(errorCode, log.ErrorCode)
		log.Error = firstNonEmpty(errorMessage, log.Error)
	}
	if log.Capability == "image" {
		if data, ok := payload["data"].([]any); ok {
			log.MediaCount = len(data)
		} else if images, ok := payload["images"].([]any); ok {
			log.MediaCount = len(images)
		}
	}
}

func providerResponsePayloads(responseBody []byte) []map[string]any {
	if len(responseBody) == 0 {
		return nil
	}
	var payload map[string]any
	if json.Unmarshal(responseBody, &payload) == nil {
		return []map[string]any{payload}
	}
	result := make([]map[string]any, 0)
	scanner := bufio.NewScanner(bytes.NewReader(responseBody))
	scanner.Buffer(make([]byte, 64<<10), max(len(responseBody)+1, 64<<10))
	dataLines := make([]string, 0, 1)
	flush := func() {
		raw := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = dataLines[:0]
		if raw == "" || raw == "[DONE]" {
			return
		}
		var event map[string]any
		if json.Unmarshal([]byte(raw), &event) == nil {
			result = append(result, event)
		}
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
	return result
}

// providerRequestIDPathCollections 是"父段为任务集合、末段即上游任务句柄"的路径段。
// history 来自 ComfyUI 原生 /history/{prompt_id}：该响应以 prompt_id 作顶层 key，
// 正文里没有可取值的句柄字段，只能从路径回推，否则轮询与取消日志依旧带不出上游 id。
var providerRequestIDPathCollections = map[string]bool{"videos": true, "tasks": true, "history": true}

func providerRequestIDFromPath(path string) string {
	parts := strings.Split(strings.Trim(strings.TrimSpace(path), "/"), "/")
	for index := len(parts) - 1; index >= 0; index-- {
		part := strings.TrimSpace(parts[index])
		if part == "" || part == "content" || part == "download" {
			continue
		}
		if index > 0 && providerRequestIDPathCollections[parts[index-1]] {
			return part
		}
		break
	}
	return ""
}

func firstInt64(values map[string]any, keys ...string) int64 {
	value, _ := firstInt64Value(values, keys...)
	return value
}

func firstInt64Value(values map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		switch value := values[key].(type) {
		case float64:
			return int64(value), true
		case int64:
			return value, true
		case json.Number:
			parsed, err := value.Int64()
			return parsed, err == nil
		}
	}
	return 0, false
}
