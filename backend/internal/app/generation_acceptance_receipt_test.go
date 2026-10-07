package app

// 声明式受理回执的宿主侧测试：上游接受 create 之后，provider 请求 id 必须同步落到
// provider_request_id，并与既有的 api_call_logs 回灌共存——两边写同一列、同一语义，
// 调度字段（poll_stage / next_poll_at）归回灌与轮询循环所有，不能被回执覆盖。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"infinite-canvas/backend/internal/generation"
	"infinite-canvas/backend/internal/model"
	"infinite-canvas/backend/internal/platform"
	"infinite-canvas/backend/internal/protocol"
)

func acceptanceReceiptFixture(t *testing.T) (*Service, *gorm.DB, model.Task) {
	t.Helper()
	s, db := newTimelineTaskTestService(t)
	// 本地模式让调用日志走自用的运行策略，回灌链路在测试库里可完整跑通。
	s.mode = serviceModeLocal
	s.coordinator = platform.NewLocalCoordinator()
	task := model.Task{ID: "acceptance-task", UserID: "acceptance-user", Type: "canvas_image", Status: model.TaskStatusRunning, InputJSON: `{"mode":"image"}`}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	return s, db, task
}

func TestAcceptanceReceiptKeepsSchedulingFieldsAndIsIdempotent(t *testing.T) {
	s, db, task := acceptanceReceiptFixture(t)
	next := time.Now().Add(30 * time.Second)
	if err := s.repo.UpdateTaskProviderState(task.ID, "", "poll", &next); err != nil {
		t.Fatal(err)
	}
	before, err := s.repo.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withProviderAnalytics(context.Background(), s, task)
	port := appReceiptPort{service: s}
	// 重入与重试：同一条上游句柄反复回执只能得到同一个结果。
	for round := 0; round < 3; round++ {
		if err := port.RecordAccepted(ctx, " pid-7 "); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	stored, err := s.repo.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ProviderRequestID != "pid-7" {
		t.Fatalf("受理句柄未落库：ProviderRequestID=%q", stored.ProviderRequestID)
	}
	if stored.PollStage != before.PollStage {
		t.Fatalf("受理回执覆盖了调度阶段：%q != %q", stored.PollStage, before.PollStage)
	}
	if stored.NextPollAt == nil || !stored.NextPollAt.Equal(*before.NextPollAt) {
		t.Fatalf("受理回执覆盖了下次轮询时间：%v != %v", stored.NextPollAt, before.NextPollAt)
	}
	// 回灌随后写入同一条句柄时也必须保持不变（不互相覆盖）。
	if err := s.repo.UpdateTaskProviderState(task.ID, "pid-7", "accepted", nil); err != nil {
		t.Fatal(err)
	}
	var after model.Task
	if err := db.First(&after, "id = ?", task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if after.ProviderRequestID != "pid-7" || after.PollStage != "accepted" {
		t.Fatalf("回灌之后受理句柄丢失：%+v", after)
	}
}

// 回执端口对"无本地任务"和"写不进本地任务"必须给出不同答案：渠道模型试跑、独立生成
// 这类调用面本就没有任务可回执，是空操作；一旦带着任务身份却找不到归属，就是真实故障，
// 必须向上抛，让调用点转换成提交结果未知。
func TestAcceptanceReceiptFailsClosedForTaskBoundContextOnly(t *testing.T) {
	s, db, task := acceptanceReceiptFixture(t)
	ctx := withProviderAnalytics(context.Background(), s, task)
	port := appReceiptPort{service: s}

	if err := port.RecordAccepted(ctx, "   "); err == nil || !strings.Contains(err.Error(), "缺少上游任务 ID") {
		t.Fatalf("空句柄必须失败，got %v", err)
	}
	noTaskCtx := s.bindGenerationRuntime(context.Background(), generation.CallMeta{UserID: task.UserID, Capability: "image"})
	if err := port.RecordAccepted(noTaskCtx, "pid-noop"); err != nil {
		t.Fatalf("无本地任务的调用面应为空操作，got %v", err)
	}
	foreignCtx := s.bindGenerationRuntime(context.Background(), generation.CallMeta{UserID: "another-user", TaskID: task.ID, Capability: "image"})
	if err := port.RecordAccepted(foreignCtx, "pid-foreign"); err == nil {
		t.Fatal("跨用户任务句柄必须被归属校验拒绝")
	}
	stored, err := s.repo.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ProviderRequestID != "" {
		t.Fatalf("失败的回执不得写入任务表：%q", stored.ProviderRequestID)
	}
	if err := (appReceiptPort{}).RecordAccepted(ctx, "pid-nil-service"); err == nil {
		t.Fatal("带任务身份却没有可写的本地库时必须失败")
	}
	var logs int64
	if err := db.Model(&model.ApiCallLog{}).Where("task_id = ?", task.ID).Count(&logs).Error; err != nil {
		t.Fatal(err)
	}
	if logs != 0 {
		t.Fatalf("端口级测试不应产生调用日志：%d", logs)
	}
}

// 端到端：provider 只在受理响应的嵌套字段里给句柄（回灌的候选键取不到）时，声明式执行链
// 也要在第一次轮询之前把句柄写进任务表，并且不吞掉 create/poll 两条调用日志。
func TestDeclarativeAcceptanceReceiptRecordsBeforeFirstPoll(t *testing.T) {
	allowLoopbackProviderTest(t)
	s, db, task := acceptanceReceiptFixture(t)
	adapter, err := protocol.LoadManifest([]byte(`{
		"apiVersion":"beeftv.plugin/v1",
		"id":"test-acceptance-nested-handle","version":"1.0.0","name":"Test Acceptance Nested Handle","author":"Test","documentation":"# Test",
		"contributes":{"providers":[{
			"id":"test-acceptance-nested-handle","label":"Test Acceptance Nested Handle","capabilities":["image"],"scopes":["canvas"],
			"create":{"method":"POST","path":"/prompt","fields":{"prompt":"request.prompt"}},
			"poll":{"method":"GET","path":"/history/{{taskId}}"},
			"response":{"taskIdPaths":["result.prompt_id"],"statusPaths":["status"],"resultPaths":["image_url"],"resultKind":"image"}
		}]}
	}`))
	if err != nil {
		t.Fatal(err)
	}

	var handleAtFirstPoll string
	polls := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/prompt":
			// 回灌候选键取不到的形态：句柄只藏在 result.prompt_id。
			_, _ = w.Write([]byte(`{"result":{"prompt_id":"pid-only-nested"},"number":1}`))
		case "/v1/history/pid-only-nested":
			polls++
			if stored, loadErr := s.repo.Task(task.ID); loadErr == nil && handleAtFirstPoll == "" {
				handleAtFirstPoll = stored.ProviderRequestID
			}
			_, _ = w.Write([]byte(`{"status":"succeeded","image_url":"` + server.URL + `/v1/view"}`))
		case "/v1/view":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("png-bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ctx := withProviderAnalytics(context.Background(), s, task)
	policy := declarativeProtocolPollPolicy("image")
	policy.Sleep = func(context.Context, time.Duration) error { return nil }
	result, err := runProtocolAdapterTaskWithPolicy(ctx, canvasGenerationInput{
		Mode:   "image",
		Prompt: "draw a cat",
		Config: providerConfig{BaseURL: server.URL + "/v1", APIKey: "key", Model: "test-model", InterfaceType: adapter.Metadata().ID},
	}, adapter, policy)
	if err != nil {
		t.Fatal(err)
	}
	if result["mode"] != "image" || polls == 0 {
		t.Fatalf("result = %#v, polls = %d", result, polls)
	}
	if handleAtFirstPoll != "pid-only-nested" {
		t.Fatalf("第一次轮询时任务表还没有上游句柄：%q", handleAtFirstPoll)
	}
	stored, err := s.repo.Task(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 后续的 poll 回灌不得把受理句柄覆盖成空或改成别的值。
	if stored.ProviderRequestID != "pid-only-nested" {
		t.Fatalf("轮询之后受理句柄丢失：%q", stored.ProviderRequestID)
	}
	var callLogs []model.ApiCallLog
	if err := db.Where("task_id = ?", task.ID).Order("created_at asc").Find(&callLogs).Error; err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, 0, len(callLogs))
	for _, callLog := range callLogs {
		kinds = append(kinds, callLog.RequestKind)
	}
	joined := strings.Join(kinds, ",")
	if !strings.Contains(joined, "create") || !strings.Contains(joined, "poll") {
		t.Fatalf("受理回执不能吞掉 api_call_logs 回灌，got kinds=%q", joined)
	}
}
