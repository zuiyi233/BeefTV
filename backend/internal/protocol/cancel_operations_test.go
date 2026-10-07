package protocol

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// cancel 与 cancelOperations 用来表达"一次调用覆盖不全"的协议：ComfyUI 的
// /queue delete 只摘掉排队项，执行中的任务还要 /interrupt。这里锁住声明顺序、
// 校验规则，以及响应映射能拿到 baseUrl。

func manifestCancelProvider(cancel *ManifestOperation, followups ...*ManifestOperation) ManifestProvider {
	return ManifestProvider{
		ID:               "cancel-probe",
		Label:            "Cancel Probe",
		Capabilities:     []Capability{CapabilityImage},
		Scopes:           []Surface{"canvas"},
		Create:           ManifestOperation{Method: "POST", Path: "/prompt"},
		Cancel:           cancel,
		CancelOperations: followups,
	}
}

func TestValidateManifestRequiresCancelForCancelOperations(t *testing.T) {
	provider := manifestCancelProvider(nil, &ManifestOperation{Method: "POST", Path: "/interrupt"})
	manifest := Manifest{
		APIVersion: "beeftv.plugin/v2",
		Metadata:   Metadata{ID: "cancel-probe-plugin", Name: "Cancel Probe", Version: "1.0.0"},
	}
	manifest.Contributes.Providers = []ManifestProvider{provider}

	err := ValidateManifest(manifest)
	if err == nil || !strings.Contains(err.Error(), "cancelOperations requires a cancel operation") {
		t.Fatalf("cancelOperations without cancel must be rejected, got %v", err)
	}
}

func TestValidateManifestRejectsMalformedCancelOperation(t *testing.T) {
	provider := manifestCancelProvider(&ManifestOperation{Method: "POST", Path: "/queue"}, &ManifestOperation{Method: "POST"})
	manifest := Manifest{
		APIVersion: "beeftv.plugin/v2",
		Metadata:   Metadata{ID: "cancel-probe-plugin", Name: "Cancel Probe", Version: "1.0.0"},
	}
	manifest.Contributes.Providers = []ManifestProvider{provider}

	err := ValidateManifest(manifest)
	if err == nil || !strings.Contains(err.Error(), "cancel operation 0") {
		t.Fatalf("malformed follow-up cancel operation must be reported with its index, got %v", err)
	}
}

func TestBuildCancelRequestsRunsInDeclaredOrder(t *testing.T) {
	adapter := manifestAdapter{manifest: Manifest{
		Metadata: Metadata{ID: "comfyui-probe"},
		Create:   ManifestOperation{Method: "POST", Path: "/prompt"},
		Cancel: &ManifestOperation{
			Method:      "POST",
			Path:        "/queue",
			ContentType: "application/json",
			// body 走表达式树，{{taskId}} 只在 path 上替换，这里必须是 $ref。
			Body: map[string]any{"delete": []any{map[string]any{"$ref": "taskId"}}},
		},
		CancelOperations: []*ManifestOperation{
			{Method: "POST", Path: "/interrupt", ContentType: "application/json", Body: map[string]any{"prompt_id": map[string]any{"$ref": "taskId"}}},
		},
	}}

	specs, err := adapter.BuildCancelRequests(context.Background(), PollContext{
		BaseURL: "http://127.0.0.1:8188",
		TaskID:  "pid-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("expected queue delete then interrupt, got %d specs", len(specs))
	}
	if specs[0].Path != "/queue" || specs[1].Path != "/interrupt" {
		t.Fatalf("declaration order must be preserved: %q then %q", specs[0].Path, specs[1].Path)
	}
	for index, spec := range specs {
		body, err := json.Marshal(spec.Body)
		if err != nil {
			t.Fatalf("marshal spec %d body: %v", index, err)
		}
		if !strings.Contains(string(body), "pid-1") {
			t.Fatalf("spec %d must carry the upstream task id: %s", index, body)
		}
		if err := spec.Validate(); err != nil {
			t.Fatalf("spec %d invalid: %v", index, err)
		}
	}
}

func TestBuildCancelRequestsWithoutFollowupsReturnsSingleSpec(t *testing.T) {
	adapter := manifestAdapter{manifest: Manifest{
		Cancel: &ManifestOperation{Method: "DELETE", Path: "/tasks/{{taskId}}"},
	}}
	specs, err := adapter.BuildCancelRequests(context.Background(), PollContext{TaskID: "t-9"})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Path != "/tasks/t-9" {
		t.Fatalf("single-cancel protocols must keep their shape, got %#v", specs)
	}

	// 未声明 cancel 的协议必须显式失败，不能退化成空操作。
	none := manifestAdapter{manifest: Manifest{}}
	if specs, err := none.BuildCancelRequests(context.Background(), PollContext{TaskID: "t-9"}); err == nil || len(specs) != 0 {
		t.Fatalf("unsupported cancellation must fail: specs=%#v err=%v", specs, err)
	}
}

func TestResponseMappingBuildsAbsoluteURLFromBaseURL(t *testing.T) {
	adapter := manifestAdapter{manifest: Manifest{
		Response: ManifestResponse{
			StatusPaths: []string{"status"},
			Images: map[string]any{"$concat": []any{
				map[string]any{"$ref": "baseUrl"},
				map[string]any{"$literal": "/view?filename="},
				map[string]any{"$urlencode": map[string]any{"$ref": "response.file"}},
			}},
		},
	}}

	result := adapter.parse(map[string]any{"status": "SUCCESS", "file": "中文 #1.png"}, PollContext{BaseURL: "http://127.0.0.1:8188"})
	if result.Result == nil || len(result.Result.Images) != 1 {
		t.Fatalf("images = %#v", result.Result)
	}
	got := result.Result.Images[0].URL
	// 与 Go provider 用 url.Values.Encode() 生成的查询串逐字节一致：空格是 +，# 转义。
	want := "http://127.0.0.1:8188/view?filename=%E4%B8%AD%E6%96%87+%231.png"
	if got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}
}
