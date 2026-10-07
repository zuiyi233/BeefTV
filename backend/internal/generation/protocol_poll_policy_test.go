package generation

// 声明式 provider 的 poll.policy 接线测试：插件声明的间隔必须真的驱动宿主轮询循环，
// 未声明时宿主的既有节奏（图片 2500ms、视频 30s）必须一字不变。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"infinite-canvas/backend/internal/protocol"
)

func pollPolicyManifest(providerID string, policy string) []byte {
	poll := `{"method":"GET","path":"/tasks/{{taskId}}"}`
	if policy != "" {
		poll = `{"method":"GET","path":"/tasks/{{taskId}}","policy":` + policy + `}`
	}
	return []byte(`{
		"apiVersion":"beeftv.plugin/v1",
		"id":"` + providerID + `","version":"1.0.0","name":"` + providerID + `","author":"Test","documentation":"# Test",
		"contributes":{"providers":[{
			"id":"` + providerID + `","label":"` + providerID + `","capabilities":["image"],"scopes":["canvas"],
			"create":{"method":"POST","path":"/tasks","fields":{"model":"request.model","prompt":"request.prompt"}},
			"poll":` + poll + `,
			"response":{"taskIdPaths":["id"],"statusPaths":["status"],"resultPaths":["image_url"],"resultKind":"image"}
		}]}
	}`)
}

// pollPolicyShape 只包含宿主调度读取的数值与开关，刻意不含 Sleep/Notify 这两个函数指针，
// 这样两个策略是否“同一节奏”可以直接比较。
func pollPolicyShape(policy VideoPollPolicy) string {
	return fmt.Sprintf("initial=%s interval=%s notFound=%d malformed=%d download=%d retryTransient=%v",
		policy.InitialDelay, policy.Interval, policy.MaxNotFoundMisses, policy.MaxMalformedResponses,
		policy.MaxDownloadTries, policy.RetryTransient)
}

// undeclaredTestAdapter 不实现 protocol.PollPolicyAdapter，模拟没有 poll.policy 的 provider。
type undeclaredTestAdapter struct{}

func (undeclaredTestAdapter) Metadata() protocol.Metadata {
	return protocol.Metadata{ID: "undeclared-poll-policy", Execution: "declarative"}
}
func (undeclaredTestAdapter) BuildCreate(context.Context, protocol.RequestContext) (protocol.RequestSpec, error) {
	return protocol.RequestSpec{}, nil
}
func (undeclaredTestAdapter) ParseCreate(context.Context, []byte) (protocol.CreateResult, error) {
	return protocol.CreateResult{}, nil
}
func (undeclaredTestAdapter) BuildPoll(context.Context, protocol.PollContext) (protocol.RequestSpec, error) {
	return protocol.RequestSpec{}, nil
}
func (undeclaredTestAdapter) ParsePoll(context.Context, protocol.PollContext, []byte) (protocol.PollResult, error) {
	return protocol.PollResult{}, nil
}
func (undeclaredTestAdapter) BuildCancel(context.Context, protocol.PollContext) (protocol.RequestSpec, error) {
	return protocol.RequestSpec{}, nil
}

// declaredTestAdapter 直接实现 protocol.PollPolicyAdapter，用于覆盖协议层校验不到的取值
// （0、负数），锁定宿主把它们当作“未声明”。
type declaredTestAdapter struct {
	policy   protocol.PollPolicy
	reported bool
}

func (a declaredTestAdapter) Metadata() protocol.Metadata {
	return protocol.Metadata{ID: "declared-poll-policy", Execution: "declarative"}
}
func (a declaredTestAdapter) BuildCreate(context.Context, protocol.RequestContext) (protocol.RequestSpec, error) {
	return protocol.RequestSpec{}, nil
}
func (a declaredTestAdapter) ParseCreate(context.Context, []byte) (protocol.CreateResult, error) {
	return protocol.CreateResult{}, nil
}
func (a declaredTestAdapter) BuildPoll(context.Context, protocol.PollContext) (protocol.RequestSpec, error) {
	return protocol.RequestSpec{}, nil
}
func (a declaredTestAdapter) ParsePoll(context.Context, protocol.PollContext, []byte) (protocol.PollResult, error) {
	return protocol.PollResult{}, nil
}
func (a declaredTestAdapter) BuildCancel(context.Context, protocol.PollContext) (protocol.RequestSpec, error) {
	return protocol.RequestSpec{}, nil
}
func (a declaredTestAdapter) PollPolicy() (protocol.PollPolicy, bool) {
	return a.policy, a.reported
}

func TestApplyDeclaredPollPolicyKeepsHostRhythmWhenUndeclared(t *testing.T) {
	for _, mode := range []string{"image", "video", "audio", "text", ""} {
		base := DeclarativeProtocolPollPolicy(mode)
		got := ApplyDeclaredPollPolicy(undeclaredTestAdapter{}, base)
		if pollPolicyShape(got) != pollPolicyShape(base) {
			t.Fatalf("mode %q undeclared policy = %s, want host default %s", mode, pollPolicyShape(got), pollPolicyShape(base))
		}
	}

	// 真实声明式 manifest 未写 poll.policy 时，协议层必须报“未声明”，宿主保持 2500ms。
	adapter, err := protocol.LoadManifest(pollPolicyManifest("test-poll-undeclared", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := DeclaredProtocolPollPolicy(adapter); ok {
		t.Fatal("manifest without poll.policy reported a declared policy")
	}
	base := DeclarativeProtocolPollPolicy("image")
	got := ApplyDeclaredPollPolicy(adapter, base)
	if got.Interval != 2500*time.Millisecond {
		t.Fatalf("undeclared image interval = %s, want the existing 2500ms", got.Interval)
	}
	if got.MaxNotFoundMisses != base.MaxNotFoundMisses || got.MaxDownloadTries != base.MaxDownloadTries || got.RetryTransient {
		t.Fatalf("undeclared image policy = %s, want host default %s", pollPolicyShape(got), pollPolicyShape(base))
	}
	if video := ApplyDeclaredPollPolicy(adapter, DeclarativeProtocolPollPolicy("video")); video.Interval != DefaultVideoPollInterval || video.RetryTransient != DefaultVideoPollPolicy().RetryTransient {
		t.Fatalf("undeclared video policy = %s, want host default %s", pollPolicyShape(video), pollPolicyShape(DefaultVideoPollPolicy()))
	}
}

func TestApplyDeclaredPollPolicyUsesDeclaredInterval(t *testing.T) {
	adapter, err := protocol.LoadManifest(pollPolicyManifest("test-poll-interval", `{"intervalSeconds":2,"timeoutSeconds":1800}`))
	if err != nil {
		t.Fatal(err)
	}
	declared, ok := DeclaredProtocolPollPolicy(adapter)
	if !ok || declared.IntervalSeconds != 2 || declared.TimeoutSeconds != 1800 {
		t.Fatalf("declared policy = %#v ok=%v", declared, ok)
	}

	base := DeclarativeProtocolPollPolicy("image")
	got := ApplyDeclaredPollPolicy(adapter, base)
	if got.Interval != 2*time.Second {
		t.Fatalf("declared image interval = %s, want the declared 2s", got.Interval)
	}
	// 除 Interval 外全部保持宿主的既有语义，包括首轮延迟和 Sleep 接缝。
	want := base
	want.Interval = 2 * time.Second
	if pollPolicyShape(got) != pollPolicyShape(want) {
		t.Fatalf("declared image policy = %s, want %s", pollPolicyShape(got), pollPolicyShape(want))
	}
	if got.InitialDelay != 0 {
		t.Fatalf("declared image initial delay = %s, want the host's 0", got.InitialDelay)
	}
	if reflect.ValueOf(got.Sleep).Pointer() != reflect.ValueOf(SleepContext).Pointer() {
		t.Fatal("declared interval replaced the host sleep seam")
	}

	// 视频：30s 首轮延迟是宿主的排队判断，声明只改节奏。
	video := ApplyDeclaredPollPolicy(adapter, DeclarativeProtocolPollPolicy("video"))
	if video.Interval != 2*time.Second || video.InitialDelay != DefaultVideoPollInterval {
		t.Fatalf("declared video policy = %s, want interval 2s and the host initial delay %s", pollPolicyShape(video), DefaultVideoPollInterval)
	}
}

// TestApplyDeclaredPollPolicyIgnoresDeclaredTimeout 锁住归属结论：timeoutSeconds 不接线。
// 宿主的执行窗口、worker 租约和 PollingDeadline 才是超时权威，插件既不能延长也不能收窄，
// 因此声明里除了 interval 以外的任何 knob 都不允许改变宿主节奏。
func TestApplyDeclaredPollPolicyIgnoresDeclaredTimeout(t *testing.T) {
	values := []string{`{"intervalSeconds":2}`, `{"intervalSeconds":2,"timeoutSeconds":3}`, `{"intervalSeconds":2,"timeoutSeconds":86400}`}
	var shapes []string
	for index, policy := range values {
		adapter, err := protocol.LoadManifest(pollPolicyManifest(fmt.Sprintf("test-poll-timeout-%d", index), policy))
		if err != nil {
			t.Fatal(err)
		}
		shapes = append(shapes, pollPolicyShape(ApplyDeclaredPollPolicy(adapter, DeclarativeProtocolPollPolicy("image"))))
	}
	for _, shape := range shapes[1:] {
		if shape != shapes[0] {
			t.Fatalf("declared timeoutSeconds changed the host rhythm: %q vs %q", shapes[0], shape)
		}
	}
}

func TestApplyDeclaredPollPolicyTreatsNonPositiveIntervalAsUndeclared(t *testing.T) {
	// 协议层拒绝负数与 >300，所以宿主只会从真实 manifest 收到 0（未声明）。这里直接
	// 锁住宿主对越界/零值的降级：保持原节奏，而不是造一个“看起来生效”的默认值。
	for _, declared := range []protocol.PollPolicy{{IntervalSeconds: 0, TimeoutSeconds: 1800}, {IntervalSeconds: -5}, {IntervalSeconds: 0}} {
		adapter := declaredTestAdapter{policy: declared, reported: true}
		for _, mode := range []string{"image", "video"} {
			base := DeclarativeProtocolPollPolicy(mode)
			if got := ApplyDeclaredPollPolicy(adapter, base); pollPolicyShape(got) != pollPolicyShape(base) {
				t.Fatalf("declared %#v mode %q = %s, want unchanged host default %s", declared, mode, pollPolicyShape(got), pollPolicyShape(base))
			}
		}
	}
	// 适配器自称实现了 PollPolicyAdapter 却报 false 时同样是未声明。
	adapter := declaredTestAdapter{policy: protocol.PollPolicy{IntervalSeconds: 7}, reported: false}
	base := DeclarativeProtocolPollPolicy("image")
	if got := ApplyDeclaredPollPolicy(adapter, base); got.Interval != base.Interval {
		t.Fatalf("unreported declared policy applied: %s", pollPolicyShape(got))
	}
}

// TestOutOfRangePollPolicyNeverReachesHost 锁定越界值的处理选择：由协议层校验拒绝，
// 宿主因此只需要处理“合法值或 0”，不再另设一套夹紧规则。
func TestOutOfRangePollPolicyNeverReachesHost(t *testing.T) {
	for _, policy := range []string{`{"intervalSeconds":301}`, `{"intervalSeconds":-1}`, `{"intervalSeconds":2,"timeoutSeconds":2}`} {
		_, err := protocol.LoadManifest(pollPolicyManifest("test-poll-out-of-range", policy))
		if err == nil {
			t.Fatalf("manifest declaring %s loaded; the protocol layer must reject it before the host", policy)
		}
		if !strings.Contains(err.Error(), "intervalSeconds") && !strings.Contains(err.Error(), "timeoutSeconds") {
			t.Fatalf("manifest declaring %s failed with %v, want a poll policy error", policy, err)
		}
	}
	if _, err := protocol.LoadManifest(pollPolicyManifest("test-poll-boundary", `{"intervalSeconds":300}`)); err != nil {
		t.Fatalf("boundary interval 300 must load: %v", err)
	}
}

func TestRunProtocolAdapterTaskPollsAtDeclaredInterval(t *testing.T) {
	t.Setenv("CANVAS_ALLOW_PRIVATE_UPSTREAMS", "true")

	declared, err := protocol.LoadManifest(pollPolicyManifest("test-poll-run-declared", `{"intervalSeconds":2,"timeoutSeconds":1800}`))
	if err != nil {
		t.Fatal(err)
	}
	undeclared, err := protocol.LoadManifest(pollPolicyManifest("test-poll-run-undeclared", ""))
	if err != nil {
		t.Fatal(err)
	}

	for _, item := range []struct {
		name    string
		adapter protocol.Adapter
		want    time.Duration
	}{
		{name: "declared", adapter: declared, want: 2 * time.Second},
		{name: "undeclared keeps host default", adapter: undeclared, want: 2500 * time.Millisecond},
	} {
		t.Run(item.name, func(t *testing.T) {
			var server *httptest.Server
			var polls atomic.Int32
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/tasks":
					_, _ = w.Write([]byte(`{"id":"provider-task-1","status":"pending"}`))
				case "/v1/tasks/provider-task-1":
					if polls.Add(1) < 3 {
						_, _ = w.Write([]byte(`{"id":"provider-task-1","status":"processing"}`))
						return
					}
					_, _ = w.Write([]byte(`{"id":"provider-task-1","status":"succeeded","image_url":"` + server.URL + `/out.png"}`))
				case "/out.png":
					w.Header().Set("Content-Type", "image/png")
					_, _ = w.Write([]byte("png-bytes"))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			var slept []time.Duration
			// 记录宿主真正喂给轮询循环的间隔，而不是在外面再算一次映射。
			policy := DeclarativeProtocolPollPolicy("image")
			policy.Sleep = func(_ context.Context, delay time.Duration) error {
				slept = append(slept, delay)
				return nil
			}

			input := Input{Mode: "image", Prompt: "draw a cat", Config: Config{BaseURL: server.URL + "/v1", APIKey: "key", Model: "test-model", InterfaceType: item.adapter.Metadata().ID}}
			result, err := RunProtocolAdapterTaskWithPolicy(context.Background(), input, item.adapter, policy)
			if err != nil {
				t.Fatal(err)
			}
			if result["mode"] != "image" || polls.Load() != 3 {
				t.Fatalf("result = %#v, polls = %d", result, polls.Load())
			}
			if len(slept) != 2 {
				t.Fatalf("sleep durations = %v, want two intervals between three polls", slept)
			}
			for _, delay := range slept {
				if delay != item.want {
					t.Fatalf("poll loop interval = %s, want %s (slept %v)", delay, item.want, slept)
				}
			}
		})
	}
}
