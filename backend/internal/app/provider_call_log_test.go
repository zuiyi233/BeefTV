package app

import (
	"strings"
	"testing"

	"infinite-canvas/backend/internal/model"
)

func TestEnrichAPICallLogRecordsStablePaymentRequiredCode(t *testing.T) {
	log := &model.ApiCallLog{Status: model.ApiCallStatusFailed, StatusCode: 402}
	(&Service{}).EnrichAPICallLog(log, []byte(`{"error":{"message":"insufficient balance api-key=secret"}}`))
	if log.ErrorCode != "quota_unknown" {
		t.Fatalf("ErrorCode = %q, want quota_unknown", log.ErrorCode)
	}
	if log.Error == "" || log.Error == "insufficient balance api-key=secret" || strings.Contains(log.Error, "secret") || strings.Contains(log.Error, "api-key") {
		t.Fatalf("unsafe or empty user-facing error: %q", log.Error)
	}
}

func TestCallLogKeepsSpecificFailureWithoutRawProviderEcho(t *testing.T) {
	for _, status := range []int{200, 400, 402, 451} {
		log := &model.ApiCallLog{Status: model.ApiCallStatusFailed, StatusCode: status}
		(&Service{}).EnrichAPICallLog(log, []byte(`{"error":{"code":"prompt_blocked","message":"content safety policy prompt=PRIVATE api_key=PRIVATE"}}`))
		if log.ErrorCode != "prompt_blocked" || !strings.Contains(log.Error, "内容安全审核") || strings.Contains(log.Error, "PRIVATE") {
			t.Fatalf("status %d: unsafe or generic diagnostics: %s %s", status, log.ErrorCode, log.Error)
		}
	}
}

// 声明式插件的上游受理响应各家写法不同：ComfyUI 原生协议只给 prompt_id，Agnes 视频把
// 任务句柄放在 video_id，catalog 里多数 provider 用驼峰 taskId。回灌链路必须认全这些
// 句柄，否则任务表取不到上游 id，取消与对账只能停在 uncertain。
func TestEnrichAPICallLogExtractsDeclarativeUpstreamTaskIDs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		capability string
		path       string
		body       string
		want       string
	}{
		{name: "comfyui prompt_id", capability: "image", path: "/prompt", body: `{"prompt_id":"pid-1","number":1,"node_errors":{}}`, want: "pid-1"},
		{name: "camel case taskId", capability: "video", path: "/v1/tasks", body: `{"taskId":"task-camel","status":"pending"}`, want: "task-camel"},
		{name: "snake case task_id", capability: "video", path: "/v1/tasks", body: `{"task_id":"task-snake","status":"PENDING"}`, want: "task-snake"},
		{name: "agnes video_id", capability: "video", path: "/videos", body: `{"video_id":"vid-handle-1","status":"RUNNING"}`, want: "vid-handle-1"},
		{name: "nested data taskId", capability: "video", path: "/v1/video/generations", body: `{"data":{"taskId":"nested-camel"}}`, want: "nested-camel"},
		{name: "nested data prompt id", capability: "image", path: "/prompt", body: `{"data":{"prompt_id":"nested-pid"}}`, want: "nested-pid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callLog := &model.ApiCallLog{Status: model.ApiCallStatusSucceeded, StatusCode: 200, Capability: tc.capability, Path: tc.path, RequestKind: "create"}
			(&Service{}).EnrichAPICallLog(callLog, []byte(tc.body))
			if callLog.ProviderRequestID != tc.want {
				t.Fatalf("ProviderRequestID = %q, want %q", callLog.ProviderRequestID, tc.want)
			}
		})
	}
}

// 候选键顺序是"显式任务句柄先于泛化字段"：同一响应里既有 prompt_id 又有回显请求的 id 时，
// 落库的必须是能被取消/轮询引用的那一个。
func TestEnrichAPICallLogPrefersExplicitTaskHandleOverGenericFields(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{name: "prompt_id beats id", body: `{"id":"request-echo","prompt_id":"pid-2"}`, want: "pid-2"},
		{name: "task_id beats taskId", body: `{"taskId":"camel","task_id":"snake"}`, want: "snake"},
		{name: "taskId beats id", body: `{"id":"generic-id","taskId":"camel-handle"}`, want: "camel-handle"},
		{name: "video_id beats id", body: `{"id":"generic-id","video_id":"video-handle"}`, want: "video-handle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callLog := &model.ApiCallLog{Status: model.ApiCallStatusSucceeded, StatusCode: 200, Capability: "image", Path: "/prompt", RequestKind: "create"}
			(&Service{}).EnrichAPICallLog(callLog, []byte(tc.body))
			if callLog.ProviderRequestID != tc.want {
				t.Fatalf("ProviderRequestID = %q, want %q", callLog.ProviderRequestID, tc.want)
			}
		})
	}
}

// ComfyUI 的 /history/{prompt_id} 响应把句柄用作顶层 key，正文取不到值，只能由路径回推；
// 但路径回推只认"任务集合/句柄"这一种形状，其它末段不能被当成任务 id。
func TestEnrichAPICallLogRecoversPromptIDFromHistoryPath(t *testing.T) {
	callLog := &model.ApiCallLog{Status: model.ApiCallStatusSucceeded, StatusCode: 200, Capability: "image", Path: "/history/pid-9", RequestKind: "poll"}
	(&Service{}).EnrichAPICallLog(callLog, []byte(`{"pid-9":{"status":{"status_str":"success","completed":true}}}`))
	if callLog.ProviderRequestID != "pid-9" {
		t.Fatalf("history 路径必须回推出 prompt_id，got %q", callLog.ProviderRequestID)
	}
	for _, path := range []string{"/prompt", "/view", "/v1/models", "/history", ""} {
		if got := providerRequestIDFromPath(path); got != "" {
			t.Fatalf("providerRequestIDFromPath(%q) = %q, want no id", path, got)
		}
	}
	if got := providerRequestIDFromPath("/v1/videos/video-7"); got != "video-7" {
		t.Fatalf("既有 videos 路径规则被破坏：%q", got)
	}
}
