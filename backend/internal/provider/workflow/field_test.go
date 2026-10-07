package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFieldUnmarshalDistinguishesAbsentSourceFromExplicitDefault(t *testing.T) {
	var inferred Field
	if err := json.Unmarshal([]byte(`{"nodeId":"3","fieldName":"width","fieldType":"NUMBER"}`), &inferred); err != nil {
		t.Fatal(err)
	}
	if inferred.Source != "width" {
		t.Fatalf("absent source inferred = %q, want width", inferred.Source)
	}
	if inferred.SourceConfigured() {
		t.Fatal("absent source must not mark sourceConfigured")
	}

	var explicit Field
	if err := json.Unmarshal([]byte(`{"nodeId":"3","fieldName":"width","fieldType":"NUMBER","source":""}`), &explicit); err != nil {
		t.Fatal(err)
	}
	if explicit.Source != "" {
		t.Fatalf("explicit empty source = %q, want empty", explicit.Source)
	}
	if !explicit.SourceConfigured() {
		t.Fatal("explicit source key must mark sourceConfigured")
	}

	repaired := FieldsForMode([]Field{explicit}, "image")
	if repaired[0].Source != "" {
		t.Fatalf("explicit default was overwritten with %q", repaired[0].Source)
	}
}

func TestFieldUnmarshalInfersMediaSourceFromUpstream(t *testing.T) {
	var field Field
	if err := json.Unmarshal([]byte(`{"nodeId":"2","fieldName":"image","fieldType":"image"}`), &field); err != nil {
		t.Fatal(err)
	}
	if field.Source != "referenceImage" {
		t.Fatalf("source = %q, want referenceImage", field.Source)
	}
	if !field.SourceFromUpstream {
		t.Fatal("image field without source config should bind upstream media")
	}
}

func TestUnsafeInternalFieldsAreNotSent(t *testing.T) {
	workflowJSON := map[string]interface{}{
		"9": map[string]interface{}{
			"class_type": "INT",
			"inputs":     map[string]interface{}{"value": 8.0},
		},
	}
	fields := []Field{{
		NodeID:         "9",
		FieldName:      "value",
		ClassType:      "INT",
		FieldValue:     8,
		SafeToOverride: ptr(false),
	}}
	items, err := runningHubNodeInfoWithWorkflow(fields, nil, Input{Mode: "image"}, workflowJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("unsafe INT.value was sent: %#v", items)
	}
}

func TestConnectedInputsAreNotOverridden(t *testing.T) {
	workflowJSON := map[string]interface{}{
		"1": map[string]interface{}{
			"class_type": "CLIPTextEncode",
			"inputs":     map[string]interface{}{"text": []interface{}{"2", 0.0}},
		},
	}
	fields := []Field{{
		NodeID:    "1",
		FieldName: "text",
		Source:    "prompt",
	}}
	items, err := runningHubNodeInfoWithWorkflow(fields, nil, Input{Mode: "image", Prompt: "hello"}, workflowJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("linked input was sent: %#v", items)
	}
}

func TestOutputTraversalIsRejected(t *testing.T) {
	if runningHubRelativeOutputPath("../etc/passwd") {
		t.Fatal("parent traversal must be rejected")
	}
	if runningHubRelativeOutputPath("output/../../secret.png") {
		t.Fatal("nested traversal must be rejected")
	}
	urls := runningHubOutputURLs(map[string]any{"fileUrl": "../etc/passwd"})
	for _, url := range urls {
		if strings.Contains(url, "..") {
			t.Fatalf("traversal leaked into outputs: %#v", urls)
		}
	}
	if !runningHubRelativeOutputPath("output/result.png") {
		t.Fatal("plain output path should be accepted")
	}
}

func TestRunningHubSeedUsesUint32Range(t *testing.T) {
	field := Field{NodeID: "1", FieldName: "seed", RandomEnabled: true, Max: int64(1<<53 - 1)}
	value, present, err := resolveWorkflowFieldValue(field, nil, Input{
		Config: Config{InterfaceType: string(mustRunningHubImage())},
	})
	if err != nil || !present {
		t.Fatalf("present=%v err=%v", present, err)
	}
	seed, ok := value.(int64)
	if !ok {
		t.Fatalf("seed type %T", value)
	}
	if seed < 0 || seed > 1<<32-1 {
		t.Fatalf("seed %d outside uint32", seed)
	}
}

func mustRunningHubImage() string {
	return "runninghub-workflow-image"
}

func TestOutputHostRewriteIsFixed(t *testing.T) {
	raw := "https://rh-images-1252422369.cos.ap-beijing.myqcloud.com/a.png"
	got := rewriteRunningHubOutputHost(raw)
	if got != "https://rh-images.xiaoyaoyou.com/a.png" {
		t.Fatalf("rewrite = %q", got)
	}
	other := "https://evil.example/a.png"
	if rewriteRunningHubOutputHost(other) != other {
		t.Fatal("unrelated host must stay unchanged")
	}
}

// 参考素材字段的值是宿主刚上传的文件名，解析期抓的枚举快照按构造不可能包含它；
// 但豁免只能覆盖枚举，普通枚举字段与数值范围校验必须照常拒绝。
func TestUploadedMediaFieldsSkipOptionValidation(t *testing.T) {
	options := []interface{}{"known-a.png", "known-b.png"}
	uploaded := Field{NodeID: "10", FieldName: "image", FieldType: "COMBO", Source: "referenceImage", Options: options}
	if err := validateRunningHubWorkflowFieldValue(uploaded, "reference-uploaded.png"); err != nil {
		t.Fatalf("刚上传的参考素材被枚举校验拒绝：%v", err)
	}

	static := Field{NodeID: "12", FieldName: "preset", FieldType: "COMBO", Options: options}
	if err := validateRunningHubWorkflowFieldValue(static, "not-an-option"); err == nil || !strings.Contains(err.Error(), "不在允许选项中") {
		t.Fatalf("普通枚举字段必须继续拒绝未知值，得到 %v", err)
	}
	if err := validateRunningHubWorkflowFieldValue(static, "known-a.png"); err != nil {
		t.Fatalf("合法枚举值被拒绝：%v", err)
	}

	numeric := Field{NodeID: "13", FieldName: "steps", FieldType: "INT", Min: 1, Max: 10}
	if err := validateRunningHubWorkflowFieldValue(numeric, 40); err == nil {
		t.Fatal("超出范围的数值被接受")
	}

	for _, source := range []string{"referenceImage", "reference_image", "IMAGE", "referencevideos", "audio", "mask"} {
		if !workflowFieldUsesUploadedMedia(Field{FieldName: "x", Source: source}) {
			t.Fatalf("来源 %q 应绑定宿主上传素材", source)
		}
	}
	if workflowFieldUsesUploadedMedia(Field{FieldName: "text", Source: "prompt"}) {
		t.Fatal("提示词来源不应视为上传素材")
	}
}
