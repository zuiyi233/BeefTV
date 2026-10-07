package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"infinite-canvas/backend/internal/model"
	"infinite-canvas/backend/internal/modelcatalog"
	"infinite-canvas/backend/internal/provider/workflow"
)

func comfyUIWorkflowJSON() map[string]interface{} {
	return map[string]interface{}{
		"6": map[string]interface{}{"class_type": "CLIPTextEncode", "inputs": map[string]interface{}{"text": "x"}},
	}
}

func comfyUIAppConfig(baseURL string) providerConfig {
	return providerConfig{
		InterfaceType: string(model.ChannelInterfaceComfyUIImage),
		BaseURL:       baseURL,
		WorkflowJSON:  comfyUIWorkflowJSON(),
	}
}

// 宿主的接口门禁是插件映射与工作流分派唯一的真相来源；ComfyUI 必须同时进入
// 工作流 provider、插件映射与 modelcatalog 的工作流类型判定。
func TestComfyUIAppInterfaceGateAndPluginMapping(t *testing.T) {
	for _, value := range []string{string(model.ChannelInterfaceComfyUIImage), string(model.ChannelInterfaceComfyUIVideo)} {
		if !isComfyUIInterface(value) {
			t.Fatalf("%s 应识别为 ComfyUI 接口", value)
		}
		if !isWorkflowProviderInterface(value) {
			t.Fatalf("%s 应识别为工作流 provider", value)
		}
		pluginID, ok := workflowPluginIDForInterface(value)
		if !ok || pluginID != WorkflowPluginComfyUI {
			t.Fatalf("%s 插件映射 = %q/%v", value, pluginID, ok)
		}
		if !modelcatalog.IsWorkflowInterfaceType(value) {
			t.Fatalf("%s 应属于工作流渠道类型", value)
		}
	}
	// 音频尚未接入，不能被宿主当成工作流 provider 或 ComfyUI 渠道。
	for _, value := range []string{"comfyui-workflow-audio", "openai-image", ""} {
		if isComfyUIInterface(value) || isWorkflowProviderInterface(value) {
			t.Fatalf("%s 不应识别为 ComfyUI 工作流渠道", value)
		}
	}
	if supportsProviderCancellation(context.Background(), providerConfig{InterfaceType: "comfyui-workflow-audio"}, "pid-1") {
		t.Fatal("未接入的接口不应声明取消能力")
	}
	for _, value := range []string{"comfyui-workflow-image", "comfyui-workflow-video"} {
		if !supportsProviderCancellation(context.Background(), providerConfig{InterfaceType: value}, "pid-1") {
			t.Fatalf("%s 应支持取消", value)
		}
	}
	taskInput := map[string]any{"config": map[string]any{"interfaceType": string(model.ChannelInterfaceComfyUIVideo)}}
	if !modelcatalog.TaskInputUsesWorkflowProvider(taskInput) {
		t.Fatal("ComfyUI 任务必须走工作流渠道，不能落进自定义渠道解析")
	}
	if err := (&Service{}).RequireWorkflowPluginForInterface("comfyui-workflow-audio"); err == nil {
		t.Fatal("不存在的插件映射必须被拒绝")
	}
}

func TestComfyUICancellationDispatchThroughProviderTask(t *testing.T) {
	allowLoopbackProviderTest(t)
	var queueBody, interruptBody map[string]any
	var queried bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/queue":
			_ = json.NewDecoder(r.Body).Decode(&queueBody)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPost && r.URL.Path == "/interrupt":
			_ = json.NewDecoder(r.Body).Decode(&interruptBody)
			_, _ = w.Write([]byte(`{}`))
		case r.URL.Path == "/queue":
			queried = true
			_, _ = w.Write([]byte(`{"queue_pending":[[1,"pid-1",{}]]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	config := comfyUIAppConfig(server.URL)
	if err := cancelProviderTask(context.Background(), config, "pid-1"); err != nil {
		t.Fatal(err)
	}
	if queueBody == nil || interruptBody == nil {
		t.Fatalf("取消必须同时精确删除队列并中断：%#v %#v", queueBody, interruptBody)
	}
	if ids, _ := queueBody["delete"].([]any); len(ids) != 1 || ids[0] != "pid-1" {
		t.Fatalf("delete body = %#v", queueBody)
	}
	if interruptBody["prompt_id"] != "pid-1" {
		t.Fatalf("interrupt 必须带 prompt_id：%#v", interruptBody)
	}
	outcome, status, err := queryProviderCancellation(context.Background(), config, "pid-1")
	if err != nil {
		t.Fatal(err)
	}
	if !queried || outcome != providerCancellationPending || status != "queued" {
		t.Fatalf("对账结果 = (%s, %s) queried=%v", outcome, status, queried)
	}
}

// ComfyUI 是自托管服务，默认部署没有鉴权：可执行判定只要求地址与工作流 JSON，
// 不要求 API Key；缺任一都必须判为不可执行。
func TestComfyUIExecutableVideoConfigNeedsNoAPIKey(t *testing.T) {
	workflowJSON := map[string]any{}
	for key, value := range comfyUIWorkflowJSON() {
		workflowJSON[key] = value
	}
	if !hasExecutableProviderVideoConfig(map[string]any{"mode": "video", "config": map[string]any{
		"interfaceType": string(model.ChannelInterfaceComfyUIVideo), "baseUrl": "http://127.0.0.1:8188", "workflowJson": workflowJSON,
	}}) {
		t.Fatal("ComfyUI 视频配置在无 API Key 时应可执行")
	}
	if hasExecutableProviderVideoConfig(map[string]any{"mode": "video", "config": map[string]any{
		"interfaceType": string(model.ChannelInterfaceComfyUIVideo), "baseUrl": "http://127.0.0.1:8188",
	}}) {
		t.Fatal("缺少工作流 JSON 的 ComfyUI 配置不可执行")
	}
	if hasExecutableProviderVideoConfig(map[string]any{"mode": "video", "config": map[string]any{
		"interfaceType": string(model.ChannelInterfaceComfyUIVideo), "workflowJson": workflowJSON,
	}}) {
		t.Fatal("缺少地址的 ComfyUI 配置不可执行")
	}
	// 对照：RunningHub 仍要求积分 API Key。
	if hasExecutableProviderVideoConfig(map[string]any{"mode": "video", "config": map[string]any{
		"interfaceType": string(model.ChannelInterfaceRunningHubVideo), "baseUrl": "https://www.runninghub.cn", "workflowId": "wf-1",
	}}) {
		t.Fatal("RunningHub 缺少 API Key 不应判为可执行")
	}
}

func TestComfyUIConfigValidationRejectsUnlistedLoopbackHost(t *testing.T) {
	// 默认拒绝本机：未通过 CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS 精确放行时，
	// 配置校验必须失败，不能把 SSRF 防护留给执行阶段。
	err := validateWorkflowProviderConfig("image", comfyUIAppConfig("http://127.0.0.1:8188"))
	if err == nil {
		t.Fatal("未加入白名单的本机地址必须被拒绝")
	}
	allowLoopbackProviderTest(t)
	if err := validateWorkflowProviderConfig("image", comfyUIAppConfig("http://127.0.0.1:8188")); err != nil {
		t.Fatalf("已放行的本机地址应通过校验：%v", err)
	}
	config := comfyUIAppConfig("http://127.0.0.1:8188")
	config.WorkflowJSON = nil
	if err := validateWorkflowProviderConfig("image", config); err == nil || !strings.Contains(err.Error(), "工作流 JSON") {
		t.Fatalf("缺少工作流 JSON 必须报错：%v", err)
	}
	// 无鉴权是自托管 ComfyUI 的前提，校验不得再要求 API Key。
	videoConfig := comfyUIAppConfig("http://127.0.0.1:8188")
	videoConfig.InterfaceType = string(model.ChannelInterfaceComfyUIVideo)
	if err := validateWorkflowProviderConfig("video", videoConfig); err != nil {
		t.Fatalf("ComfyUI 视频配置不应要求 API Key：%v", err)
	}
}

func TestComfyUIDefaultBaseURLResolvesToLoopback(t *testing.T) {
	allowLoopbackProviderTest(t)
	config, err := (&Service{}).resolveProviderConfig(providerConfig{
		InterfaceType: string(model.ChannelInterfaceComfyUIImage),
		WorkflowJSON:  comfyUIWorkflowJSON(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.BaseURL != workflow.DefaultComfyUIRootURL {
		t.Fatalf("默认地址 = %q, want %q", config.BaseURL, workflow.DefaultComfyUIRootURL)
	}
}

// 提交链路必须走宿主的受保护出站通道：未放行的回环地址不能真正建立连接。
func TestComfyUISubmitPathIsGuardedByOutboundPolicy(t *testing.T) {
	reached := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		http.NotFound(w, r)
	}))
	defer server.Close()

	s, _ := newTimelineTaskTestService(t)
	_, err := s.runComfyUIWorkflow(context.Background(), canvasGenerationInput{
		Mode: "image", Prompt: "一只猫", Config: comfyUIAppConfig(server.URL),
	})
	if err == nil {
		t.Fatal("未放行回环地址时提交必须失败")
	}
	if reached != 0 {
		t.Fatalf("被 SSRF 策略拒绝的请求不应到达上游，reached=%d", reached)
	}

	allowLoopbackProviderTest(t)
	var submitted map[string]any
	runServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/prompt":
			_ = json.NewDecoder(r.Body).Decode(&submitted)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"prompt_id":"pid-app","number":1,"node_errors":{}}`))
		case "/history/pid-app":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"pid-app":{"outputs":{"9":{"images":[{"filename":"out.png","subfolder":"","type":"output"}]}},"status":{"status_str":"success","completed":true}}}`))
		case "/view":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("png-bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer runServer.Close()

	config := comfyUIAppConfig(runServer.URL)
	config.WorkflowFields = []WorkflowField{{NodeID: "6", FieldName: "text", FieldType: "STRING", Source: "prompt"}}
	result, err := s.runComfyUIWorkflow(context.Background(), canvasGenerationInput{
		Mode: "image", Prompt: "一只猫", Config: config,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result["mode"] != "image" {
		t.Fatalf("产物分类 = %#v", result)
	}
	prompt, _ := submitted["prompt"].(map[string]interface{})
	node, _ := prompt["6"].(map[string]interface{})
	inputs, _ := node["inputs"].(map[string]interface{})
	if inputs["text"] != "一只猫" {
		t.Fatalf("宿主链路未注入提示词：%#v", inputs)
	}
}

func TestComfyUIWorkflowPluginStatusIncludesBothProviders(t *testing.T) {
	s, _ := newTimelineTaskTestService(t)
	statuses := s.WorkflowPluginStatuses()
	for _, pluginID := range []string{WorkflowPluginRunningHub, WorkflowPluginComfyUI} {
		if _, exists := statuses[pluginID]; !exists {
			t.Fatalf("工作流插件状态缺少 %s：%#v", pluginID, statuses)
		}
	}
	// 失败文案必须按插件区分来源，不能把 ComfyUI 报成 RunningHub。
	err := s.RequireWorkflowPluginForUser("user-1", string(model.ChannelInterfaceComfyUIImage))
	if err == nil || !strings.Contains(err.Error(), "ComfyUI 工作流插件") {
		t.Fatalf("ComfyUI 门禁文案 = %v", err)
	}
	if err := s.RequireWorkflowPluginForUser("user-1", string(model.ChannelInterfaceRunningHubImage)); err == nil || !strings.Contains(err.Error(), "RunningHub 工作流插件") {
		t.Fatalf("RunningHub 门禁文案 = %v", err)
	}
}
