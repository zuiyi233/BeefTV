package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"infinite-canvas/backend/internal/assistant"
)

// 宿主在「这条 durable 会话钉死的模型已经不存在」时回报 session_model_changed。
// 后端必须原样把它送到前端信封的 reason 里，并给出可行动中文说明，
// 同时 /assistant/status 仍然只描述模型与宿主本身（available:true），
// 会话能不能继续只作为附加提示，不能反过来把可用宿主报成不可用。

// turnEndLine 取出转发给浏览器的最后一条 turn_end。
func turnEndLine(t *testing.T, body string) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	for idx := len(lines) - 1; idx >= 0; idx-- {
		var event map[string]any
		if json.Unmarshal([]byte(lines[idx]), &event) != nil {
			continue
		}
		if event["type"] == "turn_end" {
			return event
		}
	}
	t.Fatalf("响应里没有 turn_end 行: %s", body)
	return nil
}

func decodeRawEnvelope(t *testing.T, body string) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("响应不是信封 JSON: %s", body)
	}
	return envelope
}

func statusSessionWarningValue(t *testing.T, env *assistantTestEnv, canvasID string) (bool, any) {
	t.Helper()
	path := "/assistant/status"
	if canvasID != "" {
		path += "?canvasId=" + canvasID
	}
	data := decodeEnvelope(t, env.call(t, http.MethodGet, path, ""))
	available, _ := data["available"].(bool)
	return available, data["sessionWarning"]
}

// 宿主用非 2xx 拒绝这轮时：reason 必须逐字保留，msg 不能是通用「宿主拒绝了这次对话」。
func TestAssistantChatRejectionKeepsSessionModelChangedReasonAndActionableMessage(t *testing.T) {
	env := newAssistantTestEnv(t, func(env *assistantTestEnv) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			env.hostHits[r.URL.Path]++
			if r.URL.Path == "/health" {
				_, _ = w.Write([]byte(`{"ok":true,"busy":false,"instance":"` + assistantInstanceProof() + `"}`))
				return
			}
			if r.URL.Path == "/chat" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"code":503,"reason":"session_model_changed"}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		})
	})
	useEnvProvider(t, "chat-completion")

	recorder := env.call(t, http.MethodPost, "/assistant/chat", `{"canvasId":"`+env.canvasID+`","message":"继续旧对话"}`)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("宿主拒绝应保留原状态码: %d %s", recorder.Code, recorder.Body.String())
	}
	envelope := decodeRawEnvelope(t, recorder.Body.String())
	if envelope["reason"] != assistant.ReasonSessionModelChanged {
		t.Fatalf("reason 必须逐字透传 session_model_changed，得到 %#v", envelope)
	}
	msg, _ := envelope["msg"].(string)
	if msg != assistant.SessionModelChangedMessage {
		t.Fatalf("msg 应为可行动中文说明，得到 %q", msg)
	}
	if strings.Contains(msg, "拒绝了这次对话") || strings.TrimSpace(msg) == "" {
		t.Fatalf("msg 不能退回通用文案: %q", msg)
	}
	if !strings.Contains(msg, "新建对话") {
		t.Fatalf("msg 必须告诉用户要新建对话: %q", msg)
	}

	// 同一原因要能在状态里读到，且 available 仍是 true。
	available, warning := statusSessionWarningValue(t, env, env.canvasID)
	if !available {
		t.Fatalf("模型与宿主都可用时 available 必须为 true")
	}
	if warning != assistant.ReasonSessionModelChanged {
		t.Fatalf("status 应带上会话提示 %s，得到 %#v", assistant.ReasonSessionModelChanged, warning)
	}
}

// 宿主把原因放在 turn_end 行里（durable 的真实形状）：原文必须继续转给浏览器，
// 状态在同一条画布上给出同一原因；这一轮正常结束后提示失效，新建会话同样让它失效。
func TestAssistantSessionModelChangedWarningTracksTurnOutcome(t *testing.T) {
	var failOldSession bool
	env := newAssistantTestEnv(t, func(env *assistantTestEnv) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			env.hostHits[r.URL.Path]++
			switch r.URL.Path {
			case "/health":
				_, _ = w.Write([]byte(`{"ok":true,"busy":false,"instance":"` + assistantInstanceProof() + `"}`))
			case "/chat":
				var body struct {
					TurnID string `json:"turnId"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TurnID == "" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/x-ndjson")
				_, _ = w.Write([]byte(`{"type":"text_delta","delta":"收到"}` + "\n"))
				end := map[string]any{"type": "turn_end", "turnId": body.TurnID, "reply": "收到",
					"toolCalls": []any{}, "proposals": []any{}, "change": nil, "sessionId": "durable-old-session"}
				if failOldSession {
					end["error"] = "checkpoint 里的模型已经不存在"
					end["errorReason"] = assistant.ReasonSessionModelChanged
				}
				line, _ := json.Marshal(end)
				_, _ = w.Write(append(line, '\n'))
			case "/sessions":
				_, _ = w.Write([]byte(`{"sessionId":"durable-new-session"}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		})
	})
	useEnvProvider(t, "chat-completion")

	send := func(message string) string {
		t.Helper()
		recorder := env.call(t, http.MethodPost, "/assistant/chat", `{"canvasId":"`+env.canvasID+`","message":"`+message+`"}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("对话应把宿主流转原转发: %d %s", recorder.Code, recorder.Body.String())
		}
		return recorder.Body.String()
	}

	failOldSession = true
	stream := send("继续旧对话")
	if end := turnEndLine(t, stream); end["errorReason"] != assistant.ReasonSessionModelChanged {
		t.Fatalf("turn_end 的 errorReason 必须原样转发，得到 %#v", end)
	}
	available, warning := statusSessionWarningValue(t, env, env.canvasID)
	if !available || warning != assistant.ReasonSessionModelChanged {
		t.Fatalf("status 要与 chat 的原因一致且仍 available，得到 available=%v warning=%#v", available, warning)
	}
	// 不带 canvasId 的调用保持原契约：不新增字段，也不因为归属信息缺失而失败。
	if plain := decodeEnvelope(t, env.call(t, http.MethodGet, "/assistant/status", "")); plain["sessionWarning"] != nil {
		t.Fatalf("未带 canvasId 时不应出现会话提示: %#v", plain)
	}
	// 别的画布读不到这条提示。
	if foreign := decodeEnvelope(t, env.call(t, http.MethodGet, "/assistant/status?canvasId=other-canvas", "")); foreign["sessionWarning"] != nil {
		t.Fatalf("会话提示不能跨画布: %#v", foreign)
	}

	// 新建会话后旧提示立即失效，不用等下一轮。
	created := decodeEnvelope(t, env.call(t, http.MethodPost, "/assistant/sessions", `{"canvasId":"`+env.canvasID+`"}`))
	if created["sessionId"] != "durable-new-session" {
		t.Fatalf("新建会话应透传宿主结果: %#v", created)
	}
	if _, warning := statusSessionWarningValue(t, env, env.canvasID); warning != nil {
		t.Fatalf("新建会话后会话提示应清除，得到 %#v", warning)
	}

	// 重新出现一次失败，再用正常结束的回合清掉：提示不能长期挂在可用宿主上。
	failOldSession = true
	send("又试了一次旧对话")
	if _, warning := statusSessionWarningValue(t, env, env.canvasID); warning != assistant.ReasonSessionModelChanged {
		t.Fatalf("失败回合后应重新出现提示，得到 %#v", warning)
	}
	failOldSession = false
	if end := turnEndLine(t, send("这一轮正常")); end["errorReason"] != nil {
		t.Fatalf("正常回合不应带 errorReason: %#v", end)
	}
	if _, warning := statusSessionWarningValue(t, env, env.canvasID); warning != nil {
		t.Fatalf("正常结束的回合应清掉旧提示，得到 %#v", warning)
	}
}

// 后端自己的原因（模型没配好）与会话级提示是两层：available:false 时不叠加会话提示。
func TestAssistantUnavailableStatusDoesNotReportSessionWarning(t *testing.T) {
	env := newAssistantTestEnv(t, func(env *assistantTestEnv) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			env.hostHits[r.URL.Path]++
			if r.URL.Path == "/health" {
				_, _ = w.Write([]byte(`{"ok":true,"busy":false,"instance":"` + assistantInstanceProof() + `"}`))
				return
			}
			if r.URL.Path == "/chat" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"code":503,"reason":"session_model_changed"}`))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		})
	})
	useEnvProvider(t, "chat-completion")
	env.call(t, http.MethodPost, "/assistant/chat", `{"canvasId":"`+env.canvasID+`","message":"继续旧对话"}`)
	if _, warning := statusSessionWarningValue(t, env, env.canvasID); warning != assistant.ReasonSessionModelChanged {
		t.Fatalf("先确认提示已经记下: %#v", warning)
	}

	for _, key := range []string{"BEEFTV_AGENT_API_KEY", "BEEFTV_AGENT_BASE_URL", "BEEFTV_AGENT_MODEL", "BEEFTV_AGENT_PROTOCOL"} {
		t.Setenv(key, "")
	}
	data := decodeEnvelope(t, env.call(t, http.MethodGet, "/assistant/status?canvasId="+env.canvasID, ""))
	if available, _ := data["available"].(bool); available {
		t.Fatal("模型未配置时状态不应可用")
	}
	if data["reason"] != assistant.ReasonModelNotConfigured {
		t.Fatalf("reason 应指向模型未配置，得到 %#v", data)
	}
	if data["sessionWarning"] != nil {
		t.Fatalf("不可用时不叠加会话提示: %#v", data)
	}
}

// 记录本身的作用域与过期只按精确键，不解析宿主正文，也不跨用户或画布串。
func TestAssistantSessionWarningStoreScopesAndExpiry(t *testing.T) {
	store := newSessionWarningStore()
	store.observeTurnEnd("user-a", "canvas-1", assistant.ReasonSessionModelChanged)
	if reason, found := store.get("user-a", "canvas-1"); !found || reason != assistant.ReasonSessionModelChanged {
		t.Fatalf("记录应能按用户与画布读回，得到 %q %v", reason, found)
	}
	for _, item := range []struct{ user, canvas string }{{"user-b", "canvas-1"}, {"user-a", "canvas-2"}, {"user-a", ""}, {"", "canvas-1"}} {
		if _, found := store.get(item.user, item.canvas); found {
			t.Fatalf("作用域越界: user=%q canvas=%q", item.user, item.canvas)
		}
	}
	// 同一画布上的正常终态清掉提示；别的画布不受影响。
	store.observeTurnEnd("user-a", "canvas-2", assistant.ReasonSessionModelChanged)
	store.observeTurnEnd("user-a", "canvas-1", "turn_timeout")
	if _, found := store.get("user-a", "canvas-1"); found {
		t.Fatal("非会话原因的终态应清除提示")
	}
	if _, found := store.get("user-a", "canvas-2"); !found {
		t.Fatal("清除不能跨画布生效")
	}
	// 与会话无关的拒绝（宿主不可达、会话忙）不能抹掉仍然成立的提示。
	store.observeRejection("user-a", "canvas-2", "host_unreachable")
	store.observeRejection("user-a", "canvas-2", "session_busy")
	if _, found := store.get("user-a", "canvas-2"); !found {
		t.Fatal("无关拒绝不得清除仍然成立的提示")
	}
	store.observeRejection("user-a", "canvas-2", assistant.ReasonSessionModelChanged)
	if _, found := store.get("user-a", "canvas-2"); !found {
		t.Fatal("拒绝里的会话原因应记下")
	}
	store.clear("user-a", "canvas-2")

	// 到期后不再提示，清理后新的观察仍然生效。
	fixed := time.Now()
	store.now = func() time.Time { return fixed }
	store.observeTurnEnd("user-a", "canvas-1", assistant.ReasonSessionModelChanged)
	store.now = func() time.Time { return fixed.Add(sessionWarningTTL + time.Minute) }
	if _, found := store.get("user-a", "canvas-1"); found {
		t.Fatal("超出 TTL 后不该继续提示")
	}
	store.now = func() time.Time { return fixed.Add(sessionWarningTTL + 2*time.Minute) }
	store.observeTurnEnd("user-a", "canvas-3", assistant.ReasonSessionModelChanged)
	if _, found := store.get("user-a", "canvas-3"); !found {
		t.Fatal("到期清理后的新观察应重新给出提示")
	}

	// 容量到顶只丢最旧的提示，不能因为它记不下而影响回合。
	bounded := newSessionWarningStore()
	boundedNow := time.Now()
	bounded.now = func() time.Time { return boundedNow }
	for idx := 0; idx < sessionWarningCapacity+8; idx++ {
		bounded.observeTurnEnd("user-a", "canvas-"+strconv.Itoa(idx), assistant.ReasonSessionModelChanged)
		boundedNow = boundedNow.Add(time.Second)
	}
	bounded.mu.Lock()
	size := len(bounded.entries)
	bounded.mu.Unlock()
	if size > sessionWarningCapacity {
		t.Fatalf("记录必须 capped，得到 %d", size)
	}
	if _, found := bounded.get("user-a", "canvas-"+strconv.Itoa(sessionWarningCapacity+7)); !found {
		t.Fatal("最近一次观察必须保留")
	}
}
