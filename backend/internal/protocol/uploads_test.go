package protocol

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// uploads[] 描述 create 之前的两步预上传：协议层只把声明展开成待执行步骤，并把宿主
// 解码后的响应折叠回请求环境。出站、SSRF、凭据与超时都在宿主，本层一次 HTTP 都不发。
// 这里锁住 foreach 的顺序对齐、每次迭代的 env 可见性、capture 留在协议内的边界，以及
// 同一批地基改动带来的 poll.policy 与 request.workflow 请求面。

func uploadProbeImages() []MediaReference {
	return []MediaReference{
		{DataURL: "data:image/png;base64,AAA", Name: "one.png", MIMEType: "image/png"},
		{DataURL: "data:image/png;base64,BBB", Name: "two.png", MIMEType: "image/png"},
		{DataURL: "data:image/png;base64,CCC", Name: "three.png", MIMEType: "image/png"},
	}
}

// uploadProbeReferenceImage 是 ComfyUI 形状的参考图上传：multipart 文件部件取自当前
// 迭代项，capture 只挑出上游决定的 name/subfolder/type。
func uploadProbeReferenceImage() ManifestUpload {
	return ManifestUpload{
		ID:      "referenceImage",
		Foreach: map[string]any{"$ref": "request.images"},
		Operation: ManifestOperation{
			Method:      "POST",
			Path:        "/upload/image",
			ContentType: "multipart/form-data",
			Files: []ManifestFilePart{{
				Name:     "image",
				Source:   map[string]any{"$ref": "item.dataUrl"},
				Filename: map[string]any{"$ref": "item.name"},
			}},
			Query: map[string]any{
				"type":      map[string]any{"$literal": "input"},
				"overwrite": map[string]any{"$literal": "true"},
				// index 与 item 必须在每次迭代可见，否则无法把捕获结果对回素材。
				"label": map[string]any{"$concat": []any{map[string]any{"$ref": "item.name"}, map[string]any{"$literal": "@"}, map[string]any{"$ref": "index"}}},
			},
		},
		Capture: map[string]any{
			"name":      map[string]any{"$ref": "response.name"},
			"subfolder": map[string]any{"$ref": "response.subfolder"},
			"type":      map[string]any{"$ref": "response.type"},
			"source":    map[string]any{"$ref": "item.name"},
		},
	}
}

func uploadProbeAdapter(uploads ...ManifestUpload) manifestAdapter {
	if len(uploads) == 0 {
		uploads = []ManifestUpload{uploadProbeReferenceImage()}
	}
	return manifestAdapter{manifest: Manifest{
		Metadata: Metadata{ID: "comfyui-probe"},
		Uploads:  uploads,
		// BuildCreate 只承认"这个 manifest 确实贡献了一个 provider"，投影字段够用
		// 但这一层校验还在，所以探针也得带上贡献声明。
		Contributes: ManifestContributions{Providers: []ManifestProvider{{
			ID: "comfyui-probe", Label: "ComfyUI Probe",
			Capabilities: []Capability{CapabilityImage}, Scopes: []Surface{"canvas"},
			Uploads: uploads,
		}}},
	}}
}

func uploadProbeRequest() GenerationRequest {
	images := uploadProbeImages()
	return GenerationRequest{Capability: CapabilityImage, Model: "sd", Images: images, Inputs: images}
}

// policyProbeAdapter 只带 poll.policy 声明，用来验证 metadataAdapter 的转发行。
func policyProbeAdapter() manifestAdapter {
	adapter := uploadProbeAdapter()
	adapter.manifest.Poll = &ManifestOperation{
		Method: "GET", Path: "/history/{{taskId}}",
		Policy: &PollPolicy{IntervalSeconds: 2, TimeoutSeconds: 1800},
	}
	return adapter
}

// uploadProbeResponses 模拟宿主解码 /upload/image 响应后的 JSON 对象。
func uploadProbeResponses(name string) map[string]any {
	return map[string]any{"name": name, "subfolder": "2026-10-01", "type": "output"}
}

func TestBuildUploadRequestsExpandsForeachInInputOrder(t *testing.T) {
	adapter := uploadProbeAdapter()
	steps, err := adapter.BuildUploadRequests(context.Background(), UploadContext{BaseURL: "http://127.0.0.1:8188", Request: uploadProbeRequest()})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 {
		t.Fatalf("steps = %d, want one per image", len(steps))
	}
	wantNames := []string{"one.png", "two.png", "three.png"}
	for index, step := range steps {
		if step.ID != "referenceImage" || step.Index != index {
			t.Fatalf("step %d = %q/%d, want referenceImage/%d", index, step.ID, step.Index, index)
		}
		spec := step.Request
		if spec.Method != "POST" || spec.Path != "/upload/image" || spec.ContentType != "multipart/form-data" {
			t.Fatalf("step %d spec = %#v", index, spec)
		}
		if err := spec.Validate(); err != nil {
			t.Fatalf("step %d: %v", index, err)
		}
		if len(spec.Files) != 1 || spec.Files[0].Filename != wantNames[index] {
			t.Fatalf("step %d files = %#v", index, spec.Files)
		}
		if spec.Files[0].Reference.DataURL == "" {
			t.Fatalf("step %d lost the media payload", index)
		}
		if got := strings.Join(spec.Query["label"], ","); got != wantNames[index]+"@"+strconv.Itoa(index) {
			t.Fatalf("step %d label = %q, item/index must be visible per iteration", index, got)
		}
		if strings.Join(spec.Query["type"], "") != "input" || strings.Join(spec.Query["overwrite"], "") != "true" {
			t.Fatalf("step %d query = %#v", index, spec.Query)
		}
	}
}

func TestBuildUploadRequestsWithoutForeachRunsOnce(t *testing.T) {
	upload := ManifestUpload{
		ID: "clientId",
		Operation: ManifestOperation{
			Method: "GET",
			Path:   "/experimental",
			Query:  map[string]any{"model": map[string]any{"$ref": "request.model"}},
		},
		Capture: map[string]any{"echoed": map[string]any{"$ref": "response.model"}},
	}
	adapter := uploadProbeAdapter(upload)
	steps, err := adapter.BuildUploadRequests(context.Background(), UploadContext{Request: uploadProbeRequest()})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Index != 0 {
		t.Fatalf("steps = %#v, want a single untitled pass", steps)
	}
	if strings.Join(steps[0].Request.Query["model"], "") != "sd" {
		t.Fatalf("query = %#v", steps[0].Request.Query)
	}

	c, err := adapter.ApplyUploadResults(context.Background(), RequestContext{}, []ManifestUploadResult{{ID: "clientId", Index: 0, Values: map[string]any{"model": "sd"}}})
	if err != nil {
		t.Fatal(err)
	}
	// 无 foreach 时 uploads.<id> 是单个对象，不是长度为 1 的数组。
	entry, ok := c.Uploads["clientId"].(map[string]any)
	if !ok || entry["echoed"] != "sd" {
		t.Fatalf("uploads = %#v", c.Uploads)
	}
}

func TestApplyUploadResultsAlignsWithInputOrder(t *testing.T) {
	adapter := uploadProbeAdapter()
	ctx := context.Background()
	steps, err := adapter.BuildUploadRequests(ctx, UploadContext{Request: uploadProbeRequest()})
	if err != nil {
		t.Fatal(err)
	}
	// 宿主可以乱序回执：捕获结果必须按下标对齐输入，而不是按回执顺序。
	results := []ManifestUploadResult{
		{ID: "referenceImage", Index: 2, Values: uploadProbeResponses("three-uploaded.png")},
		{ID: "referenceImage", Index: 0, Values: uploadProbeResponses("one-uploaded.png")},
		{ID: "referenceImage", Index: 1, Values: uploadProbeResponses("two-uploaded.png")},
	}
	c, err := adapter.ApplyUploadResults(ctx, RequestContext{Request: uploadProbeRequest()}, results)
	if err != nil {
		t.Fatal(err)
	}
	items, ok := c.Uploads["referenceImage"].([]any)
	if !ok || len(items) != len(steps) {
		t.Fatalf("uploads = %#v", c.Uploads)
	}
	want := []string{"one-uploaded.png", "two-uploaded.png", "three-uploaded.png"}
	wantSource := []string{"one.png", "two.png", "three.png"}
	for index, raw := range items {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("item %d = %#v", index, raw)
		}
		if entry["name"] != want[index] || entry["subfolder"] != "2026-10-01" || entry["type"] != "output" {
			t.Fatalf("item %d = %#v", index, entry)
		}
		if entry["source"] != wantSource[index] {
			t.Fatalf("item %d item env binding = %#v", index, entry)
		}
	}
}

func TestApplyUploadResultsCaptureSeesEarlierCaptures(t *testing.T) {
	upload := uploadProbeReferenceImage()
	// 同一 id 的已完成前缀必须对后续迭代的 capture 可见。
	upload.Capture["previous"] = map[string]any{"$get": []any{
		map[string]any{"$at": []any{map[string]any{"$ref": "uploads.referenceImage"}, 0}},
		"name",
	}}
	adapter := uploadProbeAdapter(upload)
	c, err := adapter.ApplyUploadResults(context.Background(), RequestContext{Request: uploadProbeRequest()}, []ManifestUploadResult{
		{ID: "referenceImage", Index: 0, Values: uploadProbeResponses("one-uploaded.png")},
		{ID: "referenceImage", Index: 1, Values: uploadProbeResponses("two-uploaded.png")},
		{ID: "referenceImage", Index: 2, Values: uploadProbeResponses("three-uploaded.png")},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, _ := c.Uploads["referenceImage"].([]any)
	if len(items) != 3 {
		t.Fatalf("items = %#v", items)
	}
	// 第 0 项还没有前缀，取到 nil 的字段会被省略；第 1、2 项看到第 0 项。
	if first, _ := items[0].(map[string]any); first["previous"] != nil {
		t.Fatalf("first item must not see a prefix: %#v", first)
	}
	for index := 1; index < len(items); index++ {
		entry, _ := items[index].(map[string]any)
		if entry["previous"] != "one-uploaded.png" {
			t.Fatalf("item %d prefix = %#v", index, entry)
		}
	}
}

func TestUploadStepsCarryNoPluginExpressionsToTheHost(t *testing.T) {
	// 边界锁定：宿主只拿到可执行请求与解码响应，capture 模板永不离开协议层。
	stepType := reflect.TypeOf(ManifestUploadStep{})
	if stepType.NumField() != 3 {
		t.Fatalf("ManifestUploadStep fields = %d, want ID/Index/Request only", stepType.NumField())
	}
	want := [][2]string{{"ID", "string"}, {"Index", "int"}, {"Request", "protocol.RequestSpec"}}
	for index := 0; index < stepType.NumField(); index++ {
		field := stepType.Field(index)
		if field.Name != want[index][0] || field.Type.String() != want[index][1] {
			t.Fatalf("field %d = %s %s, want %s %s", index, field.Type.String(), field.Name, want[index][0], want[index][1])
		}
	}
	resultType := reflect.TypeOf(ManifestUploadResult{})
	if resultType.NumField() != 3 {
		t.Fatalf("ManifestUploadResult fields = %d, want ID/Index/Values only", resultType.NumField())
	}

	adapter := uploadProbeAdapter()
	steps, err := adapter.BuildUploadRequests(context.Background(), UploadContext{Request: uploadProbeRequest()})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"$ref", "$get", "$concat", "capture", "Capture", "foreach"} {
		if strings.Contains(string(encoded), marker) {
			t.Fatalf("steps must be fully resolved, found %q in %s", marker, encoded)
		}
	}

	// 反向也必须成立：宿主只回 JSON 对象，协议层自己按 manifest 解释。
	c, err := adapter.ApplyUploadResults(context.Background(), RequestContext{Request: uploadProbeRequest()}, []ManifestUploadResult{
		{ID: "referenceImage", Index: 0, Values: uploadProbeResponses("one-uploaded.png")},
		{ID: "referenceImage", Index: 1, Values: uploadProbeResponses("two-uploaded.png")},
		{ID: "referenceImage", Index: 2, Values: uploadProbeResponses("three-uploaded.png")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Uploads["referenceImage"].([]any); !ok {
		t.Fatalf("uploads = %#v", c.Uploads)
	}
}

func TestApplyUploadResultsRejectsPlanMismatch(t *testing.T) {
	adapter := uploadProbeAdapter()
	ctx := context.Background()
	request := uploadProbeRequest()
	good := []ManifestUploadResult{
		{ID: "referenceImage", Index: 0, Values: uploadProbeResponses("one.png")},
		{ID: "referenceImage", Index: 1, Values: uploadProbeResponses("two.png")},
		{ID: "referenceImage", Index: 2, Values: uploadProbeResponses("three.png")},
	}
	if _, err := adapter.ApplyUploadResults(ctx, RequestContext{Request: request}, good); err != nil {
		t.Fatalf("complete plan must apply: %v", err)
	}
	cases := []struct {
		name    string
		results []ManifestUploadResult
		want    string
	}{
		{"missing step", good[:2], "has no result"},
		{"unknown id", append(append([]ManifestUploadResult{}, good...), ManifestUploadResult{ID: "mask", Index: 0}), "not declared"},
		{"index out of range", []ManifestUploadResult{
			{ID: "referenceImage", Index: 0, Values: uploadProbeResponses("one.png")},
			{ID: "referenceImage", Index: 1, Values: uploadProbeResponses("two.png")},
			{ID: "referenceImage", Index: 9, Values: uploadProbeResponses("nine.png")},
		}, "no step at index 9"},
		{"duplicate index", []ManifestUploadResult{
			{ID: "referenceImage", Index: 0, Values: uploadProbeResponses("one.png")},
			{ID: "referenceImage", Index: 0, Values: uploadProbeResponses("dup.png")},
		}, "duplicate upload result"},
	}
	for _, item := range cases {
		if _, err := adapter.ApplyUploadResults(ctx, RequestContext{Request: request}, item.results); err == nil || !strings.Contains(err.Error(), item.want) {
			t.Fatalf("%s: error = %v, want %q", item.name, err, item.want)
		}
	}

	// 未声明 uploads：原样返回，且拒绝凭空回执。
	plain := manifestAdapter{manifest: Manifest{Metadata: Metadata{ID: "plain"}}}
	c, err := plain.ApplyUploadResults(ctx, RequestContext{Request: request}, nil)
	if err != nil || c.Uploads != nil {
		t.Fatalf("undeclared uploads must pass through: c=%#v err=%v", c, err)
	}
	if _, err := plain.ApplyUploadResults(ctx, RequestContext{Request: request}, []ManifestUploadResult{{ID: "ghost"}}); err == nil {
		t.Fatal("results without any declaration must fail")
	}
}

func TestBuildUploadRequestsRequiresArrayForeach(t *testing.T) {
	cases := []struct {
		name    string
		foreach any
	}{
		{"scalar source", map[string]any{"$ref": "request.model"}},
		{"unresolved path", map[string]any{"$ref": "request.nothingHere"}},
		{"malformed operator", map[string]any{"$ref": 12}},
	}
	for _, item := range cases {
		upload := uploadProbeReferenceImage()
		upload.Foreach = item.foreach
		adapter := uploadProbeAdapter(upload)
		_, err := adapter.BuildUploadRequests(context.Background(), UploadContext{Request: uploadProbeRequest()})
		if err == nil || !strings.Contains(err.Error(), "foreach") {
			t.Fatalf("%s: error = %v", item.name, err)
		}
		if item.name != "malformed operator" && !strings.Contains(err.Error(), "must resolve to an array") {
			t.Fatalf("%s: error = %v", item.name, err)
		}
	}
}

func TestEmptyForeachProducesEmptyAlignedCapture(t *testing.T) {
	adapter := uploadProbeAdapter()
	adapter.manifest.Create = ManifestOperation{Method: "POST", Path: "/prompt", Body: map[string]any{"first": map[string]any{"$get": []any{
		map[string]any{"$at": []any{map[string]any{"$ref": "uploads.referenceImage"}, 0}},
		"name",
	}}}}
	request := GenerationRequest{Capability: CapabilityImage, Model: "sd"}
	steps, err := adapter.BuildUploadRequests(context.Background(), UploadContext{Request: request})
	if err != nil {
		t.Fatal(err)
	}
	if steps != nil {
		t.Fatalf("no images means no steps: %#v", steps)
	}
	c, err := adapter.ApplyUploadResults(context.Background(), RequestContext{Request: request}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if items, ok := c.Uploads["referenceImage"].([]any); !ok || len(items) != 0 {
		t.Fatalf("uploads = %#v", c.Uploads)
	}
	// 没有任何捕获时，create 引用取到 nil，字段整体省略，不能提交空字符串。
	spec, err := adapter.BuildCreate(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(spec.Body)
	if string(body) != "{}" {
		t.Fatalf("body = %s", body)
	}
}

func TestCapturedUploadsReachCreatePollCancelAndResultEnvironment(t *testing.T) {
	adapter := uploadProbeAdapter()
	adapter.manifest.Create = ManifestOperation{
		Method: "POST", Path: "/prompt", ContentType: "application/json",
		Body: map[string]any{
			// 两种取法都要能用：静态点号路径下标，和 $at + $get 组合。
			"filename": map[string]any{"$ref": "uploads.referenceImage.0.name"},
			"subfolder": map[string]any{"$get": []any{
				map[string]any{"$at": []any{map[string]any{"$ref": "uploads.referenceImage"}, 1}},
				"subfolder",
			}},
		},
	}
	adapter.manifest.Poll = &ManifestOperation{
		Method: "GET", Path: "/history/{{taskId}}",
		Query: map[string]any{"last": map[string]any{"$ref": "uploads.referenceImage.2.name"}},
	}
	adapter.manifest.Cancel = &ManifestOperation{
		Method: "POST", Path: "/queue", ContentType: "application/json",
		Body: map[string]any{"delete": []any{map[string]any{"$at": []any{map[string]any{"$ref": "uploads.referenceImage"}, 0}}}},
	}
	adapter.manifest.ResultOperation = &ManifestOperation{
		Method: "GET",
		PathTemplate: map[string]any{"$concat": []any{
			"/view?filename=",
			map[string]any{"$urlencode": map[string]any{"$ref": "uploads.referenceImage.0.name"}},
		}},
	}
	ctx := context.Background()
	c, err := adapter.ApplyUploadResults(ctx, RequestContext{Request: uploadProbeRequest()}, []ManifestUploadResult{
		{ID: "referenceImage", Index: 0, Values: uploadProbeResponses("one-uploaded.png")},
		{ID: "referenceImage", Index: 1, Values: uploadProbeResponses("two-uploaded.png")},
		{ID: "referenceImage", Index: 2, Values: uploadProbeResponses("three-uploaded.png")},
	})
	if err != nil {
		t.Fatal(err)
	}

	spec, err := adapter.BuildCreate(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(spec.Body)
	if !strings.Contains(string(body), `"filename":"one-uploaded.png"`) || !strings.Contains(string(body), `"subfolder":"2026-10-01"`) {
		t.Fatalf("create body = %s", body)
	}

	poll := PollContext{BaseURL: "http://127.0.0.1:8188", Model: "sd", Request: c.Request, TaskID: "pid-1", Uploads: c.Uploads}
	pollSpec, err := adapter.BuildPoll(ctx, poll)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pollSpec.Query["last"], "") != "three-uploaded.png" {
		t.Fatalf("poll query = %#v", pollSpec.Query)
	}

	cancelSpecs, err := adapter.BuildCancelRequests(ctx, poll)
	if err != nil {
		t.Fatal(err)
	}
	cancelBody, _ := json.Marshal(cancelSpecs[0].Body)
	if !strings.Contains(string(cancelBody), "one-uploaded.png") {
		t.Fatalf("cancel body = %s", cancelBody)
	}

	resultSpec, err := adapter.BuildResult(ctx, poll)
	if err != nil {
		t.Fatal(err)
	}
	if resultSpec.Path != "/view?filename=one-uploaded.png" {
		t.Fatalf("result path = %q", resultSpec.Path)
	}

	// 响应映射同样能看到捕获值：ComfyUI 的 history 以任务 id 为键。
	adapter.manifest.Response = ManifestResponse{
		Text: map[string]any{"$get": []any{
			map[string]any{"$get": []any{map[string]any{"$ref": "response"}, map[string]any{"$ref": "taskId"}}},
			"state",
		}},
	}
	decoded, err := adapter.ParsePoll(ctx, poll, []byte(`{"pid-1":{"state":"completed"},"other":{"state":"skipped"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Result == nil || decoded.Result.Text != "completed" {
		t.Fatalf("poll result = %#v", decoded.Result)
	}
}

func TestValidateManifestUploadDeclarations(t *testing.T) {
	provider := func(uploads ...ManifestUpload) ManifestProvider {
		return ManifestProvider{
			ID: "comfyui-probe", Label: "ComfyUI Probe",
			Capabilities: []Capability{CapabilityImage}, Scopes: []Surface{"canvas"},
			Create:  ManifestOperation{Method: "POST", Path: "/prompt"},
			Uploads: uploads,
		}
	}
	valid := Manifest{APIVersion: "beeftv.plugin/v2", Metadata: Metadata{ID: "comfyui-probe-plugin", Name: "ComfyUI Probe", Version: "1.0.0"}}
	valid.Contributes.Providers = []ManifestProvider{provider(uploadProbeReferenceImage())}
	if err := ValidateManifest(valid); err != nil {
		t.Fatalf("valid uploads rejected: %v", err)
	}

	cases := []struct {
		name    string
		uploads []ManifestUpload
		want    string
	}{
		{"missing id", []ManifestUpload{{Operation: ManifestOperation{Method: "POST", Path: "/upload/image"}}}, "requires a usable id"},
		{"dotted id", []ManifestUpload{{ID: "reference.image", Operation: ManifestOperation{Method: "POST", Path: "/upload/image"}}}, "requires a usable id"},
		{"duplicate id", []ManifestUpload{uploadProbeReferenceImage(), uploadProbeReferenceImage()}, "duplicate upload id"},
		{"json operation requires multipart", []ManifestUpload{{ID: "referenceImage", Operation: ManifestOperation{Method: "POST", Path: "/upload/image", Files: []ManifestFilePart{{Name: "image", Source: map[string]any{"$ref": "item.dataUrl"}}}}}}, "file parts require multipart"},
		{"missing method", []ManifestUpload{{ID: "referenceImage", Operation: ManifestOperation{Path: "/upload/image"}}}, "upload \"referenceImage\" operation"},
	}
	for _, item := range cases {
		manifest := valid
		manifest.Contributes.Providers = []ManifestProvider{provider(item.uploads...)}
		err := ValidateManifest(manifest)
		if err == nil || !strings.Contains(err.Error(), item.want) {
			t.Fatalf("%s: error = %v, want %q", item.name, err, item.want)
		}
	}
}

func TestPollPolicyIsValidOnlyWhenDeclared(t *testing.T) {
	provider := func(policy *PollPolicy) Manifest {
		manifest := Manifest{APIVersion: "beeftv.plugin/v2", Metadata: Metadata{ID: "comfyui-probe-plugin", Name: "ComfyUI Probe", Version: "1.0.0"}}
		poll := &ManifestOperation{Method: "GET", Path: "/history/{{taskId}}"}
		poll.Policy = policy
		manifest.Contributes.Providers = []ManifestProvider{{
			ID: "comfyui-probe", Label: "ComfyUI Probe",
			Capabilities: []Capability{CapabilityImage}, Scopes: []Surface{"canvas"},
			Create: ManifestOperation{Method: "POST", Path: "/prompt"},
			Poll:   poll,
		}}
		return manifest
	}
	if err := ValidateManifest(provider(nil)); err != nil {
		t.Fatalf("absent policy must validate: %v", err)
	}
	if err := ValidateManifest(provider(&PollPolicy{})); err != nil {
		t.Fatalf("omitted knobs must validate: %v", err)
	}
	if err := ValidateManifest(provider(&PollPolicy{IntervalSeconds: 2, TimeoutSeconds: 1800})); err != nil {
		t.Fatalf("declared policy must validate: %v", err)
	}
	if err := ValidateManifest(provider(&PollPolicy{IntervalSeconds: 301})); err == nil || !strings.Contains(err.Error(), "intervalSeconds") {
		t.Fatalf("interval above 300: %v", err)
	}
	if err := ValidateManifest(provider(&PollPolicy{IntervalSeconds: -1})); err == nil || !strings.Contains(err.Error(), "intervalSeconds") {
		t.Fatalf("negative interval: %v", err)
	}
	if err := ValidateManifest(provider(&PollPolicy{IntervalSeconds: 2, TimeoutSeconds: 2})); err == nil || !strings.Contains(err.Error(), "greater than intervalSeconds") {
		t.Fatalf("timeout equal to interval: %v", err)
	}
}

func TestPollPolicyAdapterReportsUndeclaredAsFalse(t *testing.T) {
	manifest := []byte(`{
		"apiVersion":"beeftv.plugin/v2",
		"id":"comfyui-wire-probe","version":"1.0.0","name":"ComfyUI Wire Probe","author":"Test",
		"contributes":{"providers":[{
			"id":"comfyui-wire-probe","label":"ComfyUI Wire Probe","capabilities":["image"],"scopes":["canvas"],
			"uploads":[{
				"id":"referenceImage",
				"foreach":{"$ref":"request.images"},
				"operation":{"method":"POST","path":"/upload/image","contentType":"multipart/form-data",
					"files":[{"name":"image","source":{"$ref":"item.dataUrl"},"filename":{"$ref":"item.name"}}],
					"query":{"type":{"$literal":"input"},"overwrite":{"$literal":"true"}}},
				"capture":{"name":{"$ref":"response.name"},"subfolder":{"$ref":"response.subfolder"},"type":{"$ref":"response.type"}}
			}],
			"create":{"method":"POST","path":"/prompt","body":{"prompt":{"$ref":"request.workflow.json"},"filename":{"$ref":"uploads.referenceImage.0.name"}}},
			"poll":{"method":"GET","path":"/history/{{taskId}}","policy":{"intervalSeconds":2,"timeoutSeconds":1800}},
			"response":{"taskIdPaths":["prompt_id"]}
		}]}
	}`)
	adapter, err := LoadManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	policy, ok := adapter.(PollPolicyAdapter).PollPolicy()
	if !ok || policy.IntervalSeconds != 2 || policy.TimeoutSeconds != 1800 {
		t.Fatalf("policy = %#v (%v)", policy, ok)
	}
	steps, err := adapter.(UploadAdapter).BuildUploadRequests(context.Background(), UploadContext{Request: GenerationRequest{
		Images: []MediaReference{{DataURL: "data:image/png;base64,AAA", Name: "one.png"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Request.Files[0].Filename != "one.png" {
		t.Fatalf("steps = %#v", steps)
	}

	// 未声明 policy 的协议必须返回 false，协议层不替宿主挑节奏。
	quiet := []byte(strings.Replace(string(manifest), `,"policy":{"intervalSeconds":2,"timeoutSeconds":1800}`, "", 1))
	if strings.Contains(string(quiet), `"policy"`) {
		t.Fatal("fixture did not strip the declared policy")
	}
	quietAdapter, err := LoadManifest(quiet)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := quietAdapter.(PollPolicyAdapter).PollPolicy(); ok {
		t.Fatal("undeclared poll policy must report false")
	}
}

func TestMetadataAdapterFallsBackWithoutUploadOrPolicySurface(t *testing.T) {
	declarative := metadataAdapter{metadata: Metadata{ID: "comfyui-probe"}, delegate: uploadProbeAdapter()}
	steps, err := declarative.BuildUploadRequests(context.Background(), UploadContext{Request: uploadProbeRequest()})
	if err != nil || len(steps) != 3 {
		t.Fatalf("declarative delegate must forward: steps=%#v err=%v", steps, err)
	}
	c, err := declarative.ApplyUploadResults(context.Background(), RequestContext{Request: uploadProbeRequest()}, []ManifestUploadResult{
		{ID: "referenceImage", Index: 0, Values: uploadProbeResponses("one.png")},
		{ID: "referenceImage", Index: 1, Values: uploadProbeResponses("two.png")},
		{ID: "referenceImage", Index: 2, Values: uploadProbeResponses("three.png")},
	})
	if err != nil || len(c.Uploads["referenceImage"].([]any)) != 3 {
		t.Fatalf("uploads = %#v err=%v", c.Uploads, err)
	}
	if _, ok := declarative.PollPolicy(); ok {
		t.Fatal("a delegate without a declared poll policy must stay undeclared")
	}
	policyBound := metadataAdapter{metadata: Metadata{ID: "comfyui-probe"}, delegate: policyProbeAdapter()}
	if policy, ok := policyBound.PollPolicy(); !ok || policy.IntervalSeconds != 2 || policy.TimeoutSeconds != 1800 {
		t.Fatalf("poll policy must be forwarded: %#v (%v)", policy, ok)
	}

	// host: 绑定的执行引擎没实现这两个可选接口时：空步骤、原样 context、policy 为 false。
	hostBound := metadataAdapter{metadata: Metadata{ID: "comfyui-probe"}, delegate: UnavailableAdapter{Info: Metadata{ID: "comfyui-probe"}}}
	steps, err = hostBound.BuildUploadRequests(context.Background(), UploadContext{Request: uploadProbeRequest()})
	if err != nil || steps != nil {
		t.Fatalf("fallback steps = %#v err = %v", steps, err)
	}
	passthrough := RequestContext{BaseURL: "http://127.0.0.1:8188", Request: uploadProbeRequest()}
	got, err := hostBound.ApplyUploadResults(context.Background(), passthrough, []ManifestUploadResult{{ID: "referenceImage", Index: 0}})
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseURL != passthrough.BaseURL || got.Uploads != nil {
		t.Fatalf("fallback must return the context untouched: %#v", got)
	}
	if _, ok := hostBound.PollPolicy(); ok {
		t.Fatal("fallback must report no poll policy")
	}
}

func TestManifestRequestExposesWorkflowPayload(t *testing.T) {
	workflow := map[string]any{
		"1": map[string]any{"class_type": "CheckpointLoaderSimple", "inputs": map[string]any{"ckpt_name": "sd.safetensors"}},
		"3": map[string]any{"class_type": "KSampler", "inputs": map[string]any{"seed": 42}},
	}
	request := GenerationRequest{Capability: CapabilityImage, Model: "sd", Workflow: &WorkflowPayload{JSON: workflow, ClientID: "client-1"}}
	values := manifestRequestValues(request)
	projected, err := evaluateManifestValue(map[string]any{"$ref": "request.workflow.json"}, map[string]any{"request": values})
	if err != nil {
		t.Fatal(err)
	}
	graph, ok := projected.(map[string]any)
	if !ok || len(graph) != 2 {
		t.Fatalf("projected = %#v", projected)
	}
	if got := manifestPathValue(graph, "3.class_type"); got != "KSampler" {
		t.Fatalf("node 3 = %#v", got)
	}
	if id := manifestPathValue(values, "workflow.clientId"); id != "client-1" {
		t.Fatalf("clientId = %#v", id)
	}

	// 缺省工作流不能污染 env：键存在但为 nil，而不是凭空多出一个空对象。
	empty := manifestRequestValues(GenerationRequest{Model: "sd"})
	value, present := empty["workflow"]
	if !present {
		t.Fatal("workflow key must be stable")
	}
	if value != nil {
		t.Fatalf("absent workflow must be nil, got %#v", value)
	}
	resolved, err := evaluateManifestValue(map[string]any{"$ref": "request.workflow.json"}, map[string]any{"request": empty})
	if err != nil || resolved != nil {
		t.Fatalf("absent workflow lookup = %#v err = %v", resolved, err)
	}
}

func TestWorkflowPayloadReachesDeclarativeCreate(t *testing.T) {
	adapter := uploadProbeAdapter()
	adapter.manifest.Create = ManifestOperation{
		Method: "POST", Path: "/prompt", ContentType: "application/json",
		Query: map[string]any{"clientId": map[string]any{"$ref": "request.workflow.clientId"}},
		Body: map[string]any{
			"client_id": map[string]any{"$ref": "request.workflow.clientId"},
			"prompt":    map[string]any{"$ref": "request.workflow.json"},
		},
	}
	request := GenerationRequest{
		Capability: CapabilityImage, Model: "sd",
		Workflow: &WorkflowPayload{JSON: map[string]any{
			"1": map[string]any{"class_type": "CheckpointLoaderSimple"},
			"3": map[string]any{"class_type": "KSampler"},
		}, ClientID: "client-1"},
	}
	spec, err := adapter.BuildCreate(context.Background(), RequestContext{BaseURL: "http://127.0.0.1:8188", Request: request})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(spec.Body)
	// 节点 id 是 1/3，不是 0 起连续键，body 归一化必须保住映射形状。
	if !strings.Contains(string(body), `"prompt":{"1":{"class_type":"CheckpointLoaderSimple"`) {
		t.Fatalf("body = %s", body)
	}
	if !strings.Contains(string(body), `"client_id":"client-1"`) {
		t.Fatalf("body = %s", body)
	}
	if strings.Join(spec.Query["clientId"], "") != "client-1" {
		t.Fatalf("query = %#v", spec.Query)
	}

	// 没有工作流时不能留下空的 prompt 字段。
	bare, err := adapter.BuildCreate(context.Background(), RequestContext{BaseURL: "http://127.0.0.1:8188", Request: GenerationRequest{Model: "sd"}})
	if err != nil {
		t.Fatal(err)
	}
	bareBody, _ := json.Marshal(bare.Body)
	if string(bareBody) != "{}" {
		t.Fatalf("bare body = %s", bareBody)
	}
	if len(bare.Query) != 0 {
		t.Fatalf("bare query = %#v", bare.Query)
	}
}
