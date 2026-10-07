package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"infinite-canvas/backend/internal/protocol"
)

func TestGeminiProviderCancellationAndConfirmation(t *testing.T) {
	t.Setenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS", "127.0.0.1")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "test-key" {
			t.Errorf("missing Gemini API key")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1beta/operations/video-1:cancel":
			requests++
			_ = json.NewEncoder(w).Encode(map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/v1beta/operations/video-1":
			_ = json.NewEncoder(w).Encode(map[string]any{"done": true, "error": map[string]any{"code": 1, "message": "Operation cancelled"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	config := providerConfig{InterfaceType: "gemini-veo", BaseURL: server.URL, APIKey: "test-key"}
	if err := cancelProviderTask(context.Background(), config, "operations/video-1"); err != nil {
		t.Fatal(err)
	}
	outcome, _, err := queryProviderCancellation(context.Background(), config, "operations/video-1")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || outcome != providerCancellationConfirmed {
		t.Fatalf("unexpected Gemini cancellation result: requests=%d outcome=%s", requests, outcome)
	}
}

func TestVolcengineArkProviderCancellationUsesDelete(t *testing.T) {
	t.Setenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS", "127.0.0.1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v3/contents/generations/tasks/task-1" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing Ark bearer token")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	config := providerConfig{InterfaceType: "volcengine-ark-video", BaseURL: server.URL + "/api/v3", APIKey: "test-key"}
	if err := cancelProviderTask(context.Background(), config, "task-1"); err != nil {
		t.Fatal(err)
	}
}

// declarativeCancellationTestContext 注入一个只描述 protocol 的上传型插件，
// 用于验证宿主确实按 manifest 声明驱动取消，而不是依赖厂商白名单。
func declarativeCancellationTestContext(t *testing.T, providerID string, withPoll bool, withCancel bool, withCancelFollowUp bool) context.Context {
	t.Helper()
	operations := `"create":{"method":"POST","path":"/v1/tasks","fields":{"model":"request.model"}}`
	if withPoll {
		operations += `,"poll":{"method":"GET","path":"/v1/tasks/{{taskId}}"}`
	}
	if withCancel {
		operations += `,"cancel":{"method":"POST","path":"/v1/tasks/{{taskId}}/cancel","fields":{"reason":"host-review"}}`
	}
	if withCancelFollowUp {
		// 一次调用覆盖不全的协议：cancel 只摘排队项，补发的 interrupt 才中断执行中的任务。
		operations += `,"cancelOperations":[{"method":"POST","path":"/v1/interrupt","fields":{"prompt_id":"taskId"}}]`
	}
	manifest := fmt.Sprintf(`{"apiVersion":"beeftv.plugin/v1","id":%q,"version":"1.0.0","name":"Declarative Cancel Test","author":"Test",`+
		`"contributes":{"providers":[{"id":%q,"label":"Declarative Cancel Test","capabilities":["video"],"scopes":["canvas"],`+
		`"auth":{"type":"bearer","field":"apiKey"},%s,"response":{"statusPaths":["status"],"taskIdPaths":["id"]}}]}}`, providerID, providerID, operations)
	adapter, err := protocol.LoadManifest([]byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := protocol.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	return withProtocolRegistry(context.Background(), registry)
}

// 取消必须由插件声明决定：声明了 cancel 的插件在没有厂商分支的情况下也要真正发出
// manifest 写明的请求（方法、路径、宿主注入的凭证和声明的 body），并在插件同时声明
// poll 时用同一份声明对账取消结果。
func TestDeclarativeProviderCancellationIssuesManifestCancelRequest(t *testing.T) {
	allowLoopbackProviderTest(t)
	var cancelRequests, pollRequests int
	var cancelPath, cancelAuthorization, cancelBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks/task-9/cancel":
			cancelRequests++
			cancelPath = r.URL.Path
			cancelAuthorization = r.Header.Get("Authorization")
			body, _ := io.ReadAll(r.Body)
			cancelBody = string(body)
			_, _ = w.Write([]byte(`{"id":"task-9","status":"cancelled"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/task-9":
			pollRequests++
			_, _ = w.Write([]byte(`{"id":"task-9","status":"cancelled"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := declarativeCancellationTestContext(t, "declarative-cancel", true, true, false)
	config := providerConfig{InterfaceType: "declarative-cancel", BaseURL: server.URL, APIKey: "test-key", Model: "test-model"}
	if !supportsProviderCancellation(ctx, config, "task-9") {
		t.Fatal("声明了 cancel 操作的插件应判定为可取消")
	}
	if err := cancelProviderTask(ctx, config, "task-9"); err != nil {
		t.Fatal(err)
	}
	if cancelRequests != 1 || cancelPath != "/v1/tasks/task-9/cancel" {
		t.Fatalf("取消请求未按 manifest 声明发出：path=%q requests=%d", cancelPath, cancelRequests)
	}
	if cancelAuthorization != "Bearer test-key" {
		t.Fatalf("取消请求凭证必须由宿主注入：%q", cancelAuthorization)
	}
	if !strings.Contains(cancelBody, `"reason":"host-review"`) {
		t.Fatalf("取消请求体应来自 manifest 声明：%q", cancelBody)
	}
	outcome, status, err := queryProviderCancellation(ctx, config, "task-9")
	if err != nil {
		t.Fatal(err)
	}
	if pollRequests != 1 || outcome != providerCancellationConfirmed || status != "cancelled" {
		t.Fatalf("声明式取消对账 = (%s, %s, polls=%d)", outcome, status, pollRequests)
	}
}

// 只声明一个 cancel 的协议保持单发，声明了 cancelOperations 的协议必须按顺序补发，
// 并且每个请求都各自过宿主的凭证注入与出站边界。
func TestDeclarativeProviderCancellationIssuesFollowUpRequests(t *testing.T) {
	allowLoopbackProviderTest(t)
	var order []string
	var interruptBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks/task-9/cancel":
			order = append(order, "cancel")
			_, _ = w.Write([]byte(`{"id":"task-9","status":"cancelled"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/interrupt":
			order = append(order, "interrupt")
			if r.Header.Get("Authorization") != "Bearer test-key" {
				t.Errorf("补发请求的凭证必须由宿主注入：%q", r.Header.Get("Authorization"))
			}
			body, _ := io.ReadAll(r.Body)
			interruptBody = string(body)
			_, _ = w.Write([]byte(`{"interrupted":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := declarativeCancellationTestContext(t, "declarative-cancel-multi", false, true, true)
	config := providerConfig{InterfaceType: "declarative-cancel-multi", BaseURL: server.URL, APIKey: "test-key", Model: "test-model"}
	if !supportsProviderCancellation(ctx, config, "task-9") {
		t.Fatal("带 cancelOperations 的插件应判定为可取消")
	}
	if err := cancelProviderTask(ctx, config, "task-9"); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "cancel" || order[1] != "interrupt" {
		t.Fatalf("取消请求必须按声明顺序发完：%#v", order)
	}
	if !strings.Contains(interruptBody, `"prompt_id":"task-9"`) {
		t.Fatalf("补发请求体应由 manifest 声明决定：%q", interruptBody)
	}
}

// 未声明 cancel 的插件必须保持显式失败，不能变成静默空操作；声明了 cancel 但上游
// 返回非 2xx 时，错误要保持既有 providerHTTPError 形状；只有 cancel 没有状态查询时，
// 对账必须停在 pending/unknown，不能伪造供应商确认。
func TestDeclarativeProviderCancellationFailuresStayExplicit(t *testing.T) {
	allowLoopbackProviderTest(t)
	var cancelRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel") {
			cancelRequests++
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	undeclaredCtx := declarativeCancellationTestContext(t, "declarative-no-cancel", true, false, false)
	undeclaredConfig := providerConfig{InterfaceType: "declarative-no-cancel", BaseURL: server.URL, APIKey: "test-key", Model: "test-model"}
	if supportsProviderCancellation(undeclaredCtx, undeclaredConfig, "task-9") {
		t.Fatal("未声明 cancel 的插件不能判为可取消")
	}
	err := cancelProviderTask(undeclaredCtx, undeclaredConfig, "task-9")
	if err == nil || !strings.Contains(err.Error(), "当前上游协议不支持取消") {
		t.Fatalf("未声明 cancel 必须显式失败，got %v", err)
	}
	if cancelRequests != 0 {
		t.Fatalf("不支持取消的协议不能发出取消请求：requests=%d", cancelRequests)
	}

	declaredCtx := declarativeCancellationTestContext(t, "declarative-cancel", true, true, false)
	declaredConfig := providerConfig{InterfaceType: "declarative-cancel", BaseURL: server.URL, APIKey: "test-key", Model: "test-model"}
	if err := cancelProviderTask(declaredCtx, declaredConfig, "task-9"); err == nil {
		t.Fatal("上游非 2xx 必须失败")
	} else if got := safeProviderLogError(err); got != "上游 HTTP 502" {
		t.Fatalf("非 2xx 错误形状 = %q", got)
	}
	if cancelRequests != 1 {
		t.Fatalf("取消请求应只发出一次：requests=%d", cancelRequests)
	}

	// 只声明 cancel、没有声明状态查询时，对账必须停在 pending/unknown 且不发任何请求。
	statusQueries := 0
	queryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		statusQueries++
		http.NotFound(w, r)
	}))
	defer queryServer.Close()
	cancelOnlyCtx := declarativeCancellationTestContext(t, "declarative-cancel-only", false, true, false)
	outcome, status, err := queryProviderCancellation(cancelOnlyCtx, providerConfig{InterfaceType: "declarative-cancel-only", BaseURL: queryServer.URL, APIKey: "test-key"}, "task-9")
	if err != nil {
		t.Fatal(err)
	}
	if outcome != providerCancellationPending || status != "unknown" || statusQueries != 0 {
		t.Fatalf("无状态查询声明时不能伪造确认：(%s, %s, queries=%d)", outcome, status, statusQueries)
	}
}
