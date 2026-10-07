package protocol

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// comfyui-local 还没有打包成 .beeftv-plugin（见该目录 README「当前状态」），
// 因此这里直接读源码 manifest，把插件声明钉在协议层契约上：真到切流阶段时，
// uploads/cancelOperations/poll.policy/request.workflow 任何一处漂移都会先在这里失败。
func TestComfyUILocalSourceManifestMatchesProtocolContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "plugin-packages", "comfyui-local", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := LoadManifest(data)
	if err != nil {
		t.Fatalf("load comfyui-local manifest: %v", err)
	}
	if got := adapter.Metadata().ID; got != "comfyui-workflow-image" {
		t.Fatalf("provider id must bind the canvas interface, got %q", got)
	}

	// 取消必须声明成两次请求：/queue 摘排队项，/interrupt 中断执行中的任务。
	multi, ok := adapter.(CancelRequestsAdapter)
	if !ok {
		t.Fatal("comfyui-local must expose CancelRequestsAdapter")
	}
	specs, err := multi.BuildCancelRequests(context.Background(), PollContext{BaseURL: "http://127.0.0.1:8188", TaskID: "p-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || specs[0].Path != "/queue" || specs[1].Path != "/interrupt" {
		t.Fatalf("cancel declarations drifted: %#v", specs)
	}

	// 轮询节奏由插件声明，宿主只读取不发明默认值。
	policyAdapter, ok := adapter.(PollPolicyAdapter)
	if !ok {
		t.Fatal("comfyui-local must expose PollPolicyAdapter")
	}
	policy, declared := policyAdapter.PollPolicy()
	if !declared || policy.IntervalSeconds != 2 {
		t.Fatalf("poll.policy must declare intervalSeconds=2, got (%#v, %v)", policy, declared)
	}

	// 预上传按参考图展开；没有参考图时零步骤。
	uploadAdapter, ok := adapter.(UploadAdapter)
	if !ok {
		t.Fatal("comfyui-local must expose UploadAdapter")
	}
	empty, err := uploadAdapter.BuildUploadRequests(context.Background(), UploadContext{BaseURL: "http://127.0.0.1:8188"})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("text2image must not upload, got %d steps", len(empty))
	}
	steps, err := uploadAdapter.BuildUploadRequests(context.Background(), UploadContext{
		BaseURL: "http://127.0.0.1:8188",
		Request: GenerationRequest{Capability: CapabilityImage, Images: []MediaReference{
			{Name: "a.png", DataURL: "data:image/png;base64,AA=="},
			{Name: "b.png", DataURL: "data:image/png;base64,BB=="},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[0].Index != 0 || steps[1].Index != 1 {
		t.Fatalf("reference uploads must expand per image in order, got %#v", steps)
	}
	if steps[0].Request.Path != "/upload/image" || steps[0].Request.ContentType != "multipart/form-data" {
		t.Fatalf("upload endpoint drifted: %#v", steps[0].Request)
	}
	// 捕获值必须回填进 create 的表达式环境，否则图生图无法把文件名交给工作流。
	created, err := adapter.BuildCreate(context.Background(), RequestContext{
		BaseURL: "http://127.0.0.1:8188",
		Request: GenerationRequest{Capability: CapabilityImage, Workflow: &WorkflowPayload{JSON: map[string]any{"3": map[string]any{"class_type": "KSampler"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(created.Body)
	if err != nil {
		t.Fatalf("marshal create body: %v", err)
	}
	if !strings.Contains(string(body), "KSampler") {
		t.Fatalf("create must submit the host-mapped workflow graph, got %s", body)
	}

	// /history 以 prompt id 为顶层键，状态判定依赖 $get；顺带锁住 manifest 里确实用到了它。
	if !strings.Contains(string(data), `"$get"`) {
		t.Fatal("manifest must resolve history entries by variable prompt id")
	}
	var probe map[string]any
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatal(err)
	}
	if probe["id"] != "comfyui-local" {
		t.Fatalf("plugin id = %v", probe["id"])
	}
}
