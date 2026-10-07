package generation

// 声明式受理回执接线测试：上游一接受 create，provider 请求 id 必须同步落到本地任务，
// 之后才允许发出第一次轮询；回执写失败不能被降级成"看起来还能安全重发"的普通错误。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"infinite-canvas/backend/internal/protocol"
)

// orderedReceipts 把"上游收到的请求"和"宿主落下的受理回执"记在同一条时间线上，
// 用来锁定受理回执确实插在 create 与第一次 poll 之间。
type orderedReceipts struct {
	mu     sync.Mutex
	events []string
	err    error
}

func (r *orderedReceipts) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *orderedReceipts) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *orderedReceipts) Observe(TransportObservation) {}
func (r *orderedReceipts) NotifyPoll(context.Context, string, error) {
}
func (r *orderedReceipts) SyncProgress(string, []byte) {}

func (r *orderedReceipts) RecordAccepted(_ context.Context, providerRequestID string) error {
	if r.err != nil {
		r.record("acceptance-failed")
		return r.err
	}
	r.record("accepted:" + providerRequestID)
	return nil
}

func acceptanceManifest(providerID string) []byte {
	return []byte(`{
		"apiVersion":"beeftv.plugin/v1",
		"id":"` + providerID + `","version":"1.0.0","name":"` + providerID + `","author":"Test","documentation":"# Test",
		"contributes":{"providers":[{
			"id":"` + providerID + `","label":"` + providerID + `","capabilities":["image"],"scopes":["canvas"],
			"create":{"method":"POST","path":"/prompt","fields":{"prompt":"request.prompt"}},
			"poll":{"method":"GET","path":"/history/{{taskId}}"},
			"response":{"taskIdPaths":["prompt_id"],"statusPaths":["status"],"resultPaths":["image_url"],"resultKind":"image"}
		}]}
	}`)
}

func acceptanceTestServer(t *testing.T, receipts *orderedReceipts, createBody string) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/prompt":
			receipts.record("create")
			_, _ = w.Write([]byte(createBody))
		case strings.HasPrefix(r.URL.Path, "/v1/history/"):
			receipts.record("poll")
			_, _ = w.Write([]byte(`{"status":"succeeded","image_url":"` + server.URL + `/v1/view"}`))
		case r.URL.Path == "/v1/view":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("png-bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func runAcceptanceTask(ctx context.Context, server *httptest.Server, adapter protocol.Adapter) (map[string]interface{}, error) {
	policy := DeclarativeProtocolPollPolicy("image")
	policy.Sleep = func(context.Context, time.Duration) error { return nil }
	input := Input{Mode: "image", Prompt: "draw a cat", Config: Config{BaseURL: server.URL + "/v1", APIKey: "key", Model: "test-model", InterfaceType: adapter.Metadata().ID}}
	return RunProtocolAdapterTaskWithPolicy(ctx, input, adapter, policy)
}

// ComfyUI 原生受理响应只给 prompt_id，宿主必须把它当成这条任务的句柄同步落库，
// 并且要发生在第一次轮询之前——取消只能引用已经写进任务表的那一个 id。
func TestProtocolAcceptanceReceiptRecordsHandleBeforeFirstPoll(t *testing.T) {
	t.Setenv("CANVAS_ALLOW_PRIVATE_UPSTREAMS", "true")
	adapter, err := protocol.LoadManifest(acceptanceManifest("test-acceptance-prompt-id"))
	if err != nil {
		t.Fatal(err)
	}
	receipts := &orderedReceipts{}
	server := acceptanceTestServer(t, receipts, `{"prompt_id":"pid-9","number":1,"node_errors":{}}`)
	ctx := WithRuntime(context.Background(), Runtime{Receipts: receipts, Call: CallMeta{UserID: "user-1", TaskID: "task-1", Capability: "image"}})

	result, err := runAcceptanceTask(ctx, server, adapter)
	if err != nil {
		t.Fatal(err)
	}
	if result["mode"] != "image" {
		t.Fatalf("result = %#v", result)
	}
	want := []string{"create", "accepted:pid-9", "poll"}
	events := receipts.snapshot()
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

// 回执写失败发生在"上游已受理、本地未记账"这一最危险的时刻：必须向上抛并转换成
// 提交结果未知（阻断自动重发），既不能继续轮询假装没事，也不能静默吞掉。
func TestProtocolAcceptanceReceiptFailureBecomesSubmissionUnknown(t *testing.T) {
	t.Setenv("CANVAS_ALLOW_PRIVATE_UPSTREAMS", "true")
	adapter, err := protocol.LoadManifest(acceptanceManifest("test-acceptance-write-failure"))
	if err != nil {
		t.Fatal(err)
	}
	receipts := &orderedReceipts{err: errors.New("database is locked")}
	server := acceptanceTestServer(t, receipts, `{"prompt_id":"pid-fail"}`)
	ctx := WithRuntime(context.Background(), Runtime{Receipts: receipts, Call: CallMeta{UserID: "user-1", TaskID: "task-1", Capability: "image"}})

	_, err = runAcceptanceTask(ctx, server, adapter)
	var unknown SubmissionUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("error = %v, want SubmissionUnknownError", err)
	}
	if !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("真实回执失败原因被吞掉：%v", err)
	}
	events := receipts.snapshot()
	if strings.Join(events, ",") != "create,acceptance-failed" {
		t.Fatalf("回执失败后不应继续轮询：%v", events)
	}
}

// 关断记账（无回执端口）的调用面本来就没有本地任务，受理回执必须是空操作，
// 不能凭空造出一条"写失败"把渠道试跑打挂。
func TestProtocolAcceptanceReceiptIsNoOpWithoutReceiptsPort(t *testing.T) {
	t.Setenv("CANVAS_ALLOW_PRIVATE_UPSTREAMS", "true")
	adapter, err := protocol.LoadManifest(acceptanceManifest("test-acceptance-no-port"))
	if err != nil {
		t.Fatal(err)
	}
	receipts := &orderedReceipts{}
	server := acceptanceTestServer(t, receipts, `{"prompt_id":"pid-noop"}`)
	ctx := WithoutCallAccounting(WithRuntime(context.Background(), Runtime{Receipts: receipts, Call: CallMeta{UserID: "user-1", TaskID: "task-1"}}))

	result, err := runAcceptanceTask(ctx, server, adapter)
	if err != nil {
		t.Fatal(err)
	}
	if result["mode"] != "image" {
		t.Fatalf("result = %#v", result)
	}
	if strings.Join(receipts.snapshot(), ",") != "create,poll" {
		t.Fatalf("无回执端口时不应记录受理：%v", receipts.snapshot())
	}
}

// provider 没声明 taskIdPaths 时的兜底取值：prompt_id / video_id 也要能被认出来，
// 但已声明协议的优先级（id 在前）保持不变。
func TestExtractProviderTaskIDAcceptsUpstreamTaskHandles(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{body: `{"prompt_id":"pid-3","number":1}`, want: "pid-3"},
		{body: `{"video_id":"video-3","status":"RUNNING"}`, want: "video-3"},
		{body: `{"data":{"prompt_id":"nested-pid"}}`, want: "nested-pid"},
		{body: `{"id":"generic-id","prompt_id":"pid-3"}`, want: "generic-id"},
		{body: `{"status":"pending"}`, want: ""},
	} {
		got, err := ExtractProviderTaskID([]byte(tc.body))
		if err != nil {
			t.Fatalf("body %s: %v", tc.body, err)
		}
		if got != tc.want {
			t.Fatalf("body %s -> %q, want %q", tc.body, got, tc.want)
		}
	}
}
