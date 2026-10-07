package generation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"infinite-canvas/backend/internal/protocol"
)

// 预上传的宿主执行侧用例：uploads[] 只在 create 之前跑，宿主按协议层给出的步骤顺序出站，
// 只把解码后的 JSON 交回 ApplyUploadResults，不解释任何表达式。这一组用例分别锁住
// 无声明零 HTTP、foreach 顺序与结果对齐、resumed 跳过、失败即返回不发 create、
// 以及出站确实走 ExecuteProtocolRequest 这条既有宿主边界（凭证、SSRF、回执、阶段）。

// uploads 探针协议：一张参考图一次上传，create 用捕获值引用第 0/1/2 步的不同字段，
// 任何步骤乱序或结果错位都会让 create 正文对不上。
const uploadsProbeManifestWithUploads = `{
	"apiVersion":"beeftv.plugin/v2",
	"id":"uploads-host-probe","version":"1.0.0","name":"Uploads Host Probe","author":"Test",
	"contributes":{"providers":[{
		"id":"uploads-host-probe","label":"Uploads Host Probe","capabilities":["text"],"scopes":["canvas"],
		"uploads":[{
			"id":"referenceImage",
			"foreach":{"$ref":"request.images"},
			"operation":{
				"method":"POST","path":"/upload/image","contentType":"multipart/form-data",
				"files":[{"name":"image","source":{"$ref":"item.dataUrl"},"filename":{"$ref":"item.name"}}],
				"query":{"tag":{"$concat":[{"$ref":"item.name"},{"$literal":"#"},{"$ref":"index"}]}}
			},
			"capture":{"name":{"$ref":"response.name"},"subfolder":{"$ref":"response.subfolder"}}
		}],
		"create":{"method":"POST","path":"/prompt","body":{
			"prompt":{"$ref":"request.prompt"},
			"first":{"$ref":"uploads.referenceImage.0.name"},
			"second":{"$ref":"uploads.referenceImage.1.name"},
			"third":{"$ref":"uploads.referenceImage.2.subfolder"}
		}},
		"poll":{"method":"GET","path":"/history"},
		"response":{"textPaths":["text"]}
	}]}
}`

const uploadsProbeManifestWithoutUploads = `{
	"apiVersion":"beeftv.plugin/v2",
	"id":"uploads-host-probe","version":"1.0.0","name":"Uploads Host Probe","author":"Test",
	"contributes":{"providers":[{
		"id":"uploads-host-probe","label":"Uploads Host Probe","capabilities":["text"],"scopes":["canvas"],
		"create":{"method":"POST","path":"/prompt","body":{"prompt":{"$ref":"request.prompt"}}},
		"poll":{"method":"GET","path":"/history"},
		"response":{"textPaths":["text"]}
	}]}
}`

type uploadsProbeOptions struct {
	failUploadAt int // 第 N 次上传返回 500
	nonJSONAt    int // 第 N 次上传返回非 JSON 正文
}

type uploadsProbe struct {
	server  *httptest.Server
	options uploadsProbeOptions
	mu      sync.Mutex
	log     []string
	tags    []string
	bodies  []string
	auths   map[string][]string
	create  map[string]any
	uploads int
}

func newUploadsProbe(t *testing.T, options uploadsProbeOptions) *uploadsProbe {
	t.Helper()
	probe := &uploadsProbe{options: options, auths: map[string][]string{}}
	probe.server = httptest.NewServer(http.HandlerFunc(probe.handle))
	t.Cleanup(probe.server.Close)
	return probe
}

func (p *uploadsProbe) handle(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 宿主仍按既有传输边界补默认版本段，探针只关心自己的路径。
	path := strings.TrimPrefix(r.URL.Path, "/v1")
	p.auths[path] = append(p.auths[path], r.Header.Get("Authorization"))
	switch path {
	case "/upload/image":
		p.uploads++
		file, header, err := r.FormFile("image")
		if err != nil {
			http.Error(w, "missing multipart file", http.StatusBadRequest)
			return
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		p.log = append(p.log, "upload:"+header.Filename)
		p.bodies = append(p.bodies, string(data))
		if p.options.failUploadAt == p.uploads {
			http.Error(w, "upload rejected", http.StatusInternalServerError)
			return
		}
		if p.options.nonJSONAt == p.uploads {
			_, _ = io.WriteString(w, "not a json receipt")
			return
		}
		tag := r.URL.Query().Get("tag")
		p.tags = append(p.tags, tag)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":      "up-" + header.Filename,
			"subfolder": "sub-" + tag,
		})
	case "/prompt":
		p.log = append(p.log, "create")
		raw, _ := io.ReadAll(r.Body)
		body := map[string]any{}
		_ = json.Unmarshal(raw, &body)
		p.create = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"created"}`)
	case "/history":
		p.log = append(p.log, "poll")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"polled"}`)
	default:
		p.log = append(p.log, "unexpected:"+r.URL.Path)
		http.NotFound(w, r)
	}
}

func (p *uploadsProbe) snapshot() ([]string, []string, []string, map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.log...), append([]string(nil), p.tags...), append([]string(nil), p.bodies...), p.create
}

func (p *uploadsProbe) authFor(path string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	entries := p.auths[path]
	if len(entries) == 0 {
		return ""
	}
	return entries[0]
}

func uploadsProbeInput(baseURL string, images int) Input {
	references := make([]Media, 0, images)
	payloads := []string{"b25l", "dHdv", "dGhyZWU="}
	names := []string{"one.png", "two.png", "three.png"}
	for index := 0; index < images; index++ {
		references = append(references, Media{
			Name: names[index], MimeType: "image/png",
			DataURL: "data:image/png;base64," + payloads[index],
		})
	}
	return Input{
		Mode: "text", Prompt: "draw", ReferenceImages: references,
		Config: Config{BaseURL: baseURL, APIKey: "probe-key", Model: "probe-model"},
	}
}

func uploadsProbeAdapter(t *testing.T, manifest string) protocol.Adapter {
	t.Helper()
	adapter, err := protocol.LoadManifest([]byte(manifest))
	if err != nil {
		t.Fatalf("load probe manifest: %v", err)
	}
	return adapter
}

func TestDeclarativeUploadsRunInForeachOrderBeforeCreate(t *testing.T) {
	t.Setenv("CANVAS_ALLOW_PRIVATE_UPSTREAMS", "true")
	probe := newUploadsProbe(t, uploadsProbeOptions{})
	adapter := uploadsProbeAdapter(t, uploadsProbeManifestWithUploads)

	result, err := RunProtocolAdapterTask(WithRuntime(context.Background(), taskRuntime()), uploadsProbeInput(probe.server.URL, 3), adapter)
	if err != nil {
		t.Fatalf("RunProtocolAdapterTask: %v", err)
	}
	if result["text"] != "created" {
		t.Fatalf("result = %#v", result)
	}
	log, tags, bodies, create := probe.snapshot()
	want := []string{"upload:one.png", "upload:two.png", "upload:three.png", "create"}
	if !reflect.DeepEqual(log, want) {
		t.Fatalf("request order = %v, want %v", log, want)
	}
	// 每次迭代都要看到自己的 item 与 index，否则捕获结果无法对回素材。
	if !reflect.DeepEqual(tags, []string{"one.png#0", "two.png#1", "three.png#2"}) {
		t.Fatalf("per-iteration bindings = %v", tags)
	}
	if !reflect.DeepEqual(bodies, []string{"one", "two", "three"}) {
		t.Fatalf("uploaded bytes = %v", bodies)
	}
	// create 引用 uploads.<id>.<下标>.<字段>：捕获必须与输入顺序逐位对齐。
	if create["first"] != "up-one.png" || create["second"] != "up-two.png" || create["third"] != "sub-three.png#2" {
		t.Fatalf("create body = %#v", create)
	}
}

func TestDeclarativeUploadsSkipWhenNothingDeclared(t *testing.T) {
	t.Run("manifest without uploads", func(t *testing.T) {
		t.Setenv("CANVAS_ALLOW_PRIVATE_UPSTREAMS", "true")
		probe := newUploadsProbe(t, uploadsProbeOptions{})
		adapter := uploadsProbeAdapter(t, uploadsProbeManifestWithoutUploads)
		if _, err := RunProtocolAdapterTask(WithRuntime(context.Background(), taskRuntime()), uploadsProbeInput(probe.server.URL, 3), adapter); err != nil {
			t.Fatal(err)
		}
		log, _, _, create := probe.snapshot()
		if !reflect.DeepEqual(log, []string{"create"}) || create["prompt"] != "draw" {
			t.Fatalf("undeclared uploads must not add any HTTP: %v %#v", log, create)
		}
	})
	t.Run("adapter without upload surface", func(t *testing.T) {
		t.Setenv("CANVAS_ALLOW_PRIVATE_UPSTREAMS", "true")
		probe := newUploadsProbe(t, uploadsProbeOptions{})
		if _, err := RunProtocolAdapterTask(WithRuntime(context.Background(), taskRuntime()), uploadsProbeInput(probe.server.URL, 3), plainProbeAdapter{}); err != nil {
			t.Fatal(err)
		}
		if log, _, _, _ := probe.snapshot(); !reflect.DeepEqual(log, []string{"create"}) {
			t.Fatalf("host-bound adapter must not be asked to upload: %v", log)
		}
	})
}

func TestDeclarativeUploadsSkippedWhenTaskResumed(t *testing.T) {
	t.Setenv("CANVAS_ALLOW_PRIVATE_UPSTREAMS", "true")
	probe := newUploadsProbe(t, uploadsProbeOptions{})
	adapter := uploadsProbeAdapter(t, uploadsProbeManifestWithUploads)
	ctx := WithRuntime(context.Background(), taskRuntime(func(runtime *Runtime) {
		runtime.Call.ProviderRequestID = "pid-resumed"
	}))
	result, err := RunProtocolAdapterTask(ctx, uploadsProbeInput(probe.server.URL, 3), adapter)
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if result["text"] != "polled" {
		t.Fatalf("result = %#v", result)
	}
	if log, _, _, _ := probe.snapshot(); !reflect.DeepEqual(log, []string{"poll"}) {
		t.Fatalf("resume must not upload or create twice: %v", log)
	}
}

func TestDeclarativeUploadFailureStopsBeforeCreate(t *testing.T) {
	cases := []struct {
		name    string
		options uploadsProbeOptions
		wantLog []string
	}{
		{"first upload rejected", uploadsProbeOptions{failUploadAt: 1}, []string{"upload:one.png"}},
		{"second upload rejected", uploadsProbeOptions{failUploadAt: 2}, []string{"upload:one.png", "upload:two.png"}},
		{"upload response is not json", uploadsProbeOptions{nonJSONAt: 1}, []string{"upload:one.png"}},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			t.Setenv("CANVAS_ALLOW_PRIVATE_UPSTREAMS", "true")
			probe := newUploadsProbe(t, item.options)
			adapter := uploadsProbeAdapter(t, uploadsProbeManifestWithUploads)
			_, err := RunProtocolAdapterTask(WithRuntime(context.Background(), taskRuntime()), uploadsProbeInput(probe.server.URL, 3), adapter)
			if err == nil || !strings.Contains(err.Error(), "预上传") {
				t.Fatalf("error = %v, want a pre-upload failure", err)
			}
			log, _, _, _ := probe.snapshot()
			if !reflect.DeepEqual(log, item.wantLog) {
				t.Fatalf("requests = %v, want %v (create must never be submitted)", log, item.wantLog)
			}
			for _, entry := range log {
				if entry == "create" || entry == "poll" {
					t.Fatalf("failed upload still reached %s", entry)
				}
			}
		})
	}
}

func TestDeclarativeUploadHostCarriesDecodedValuesOnly(t *testing.T) {
	t.Setenv("CANVAS_ALLOW_PRIVATE_UPSTREAMS", "true")
	// 上游返回的 JSON 里带一个键名恰好是 "$ref" 的字面量，还有嵌套数组和数字：
	// 宿主只把正文原样解码成 JSON 对象交回去，不挑字段也不解释任何表达式。
	receiptBodies := []string{
		`{"$ref":"literal-not-an-expression","count":2,"items":[{"k":"a"}],"name":"first-asset"}`,
		`{"$ref":"second-receipt","count":3,"items":[],"name":"second-asset"}`,
	}
	decoded := make([]map[string]any, len(receiptBodies))
	for index, body := range receiptBodies {
		if err := json.Unmarshal([]byte(body), &decoded[index]); err != nil {
			t.Fatalf("decode probe receipt %d: %v", index, err)
		}
	}
	var hitsMu sync.Mutex
	var hits []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1")
		hitsMu.Lock()
		hits = append(hits, r.Method+" "+path)
		hitsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch path {
		case "/upload/one":
			_, _ = io.WriteString(w, receiptBodies[0])
		case "/upload/two":
			_, _ = io.WriteString(w, receiptBodies[1])
		case "/prompt":
			_, _ = io.WriteString(w, `{"text":"created"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	adapter := &recordingUploadAdapter{
		steps: []protocol.ManifestUploadStep{
			{ID: "asset", Index: 0, Request: protocol.RequestSpec{Method: "POST", Path: "/upload/one", ContentType: "application/json", Body: map[string]any{"marker": "one"}}},
			{ID: "asset", Index: 1, Request: protocol.RequestSpec{Method: "POST", Path: "/upload/two", ContentType: "application/json", Body: map[string]any{"marker": "two"}}},
		},
		captured: map[string]any{"asset": []any{}},
	}
	input := uploadsProbeInput(server.URL, 2)
	if _, err := RunProtocolAdapterTask(WithRuntime(context.Background(), taskRuntime()), input, adapter); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(hits, []string{"POST /upload/one", "POST /upload/two", "POST /prompt"}) {
		t.Fatalf("outbound order = %v", hits)
	}
	if adapter.applyCalls != 1 {
		t.Fatalf("ApplyUploadResults calls = %d, want a single pass with every result", adapter.applyCalls)
	}
	want := []protocol.ManifestUploadResult{
		{ID: "asset", Index: 0, Values: decoded[0]},
		{ID: "asset", Index: 1, Values: decoded[1]},
	}
	if !reflect.DeepEqual(adapter.applied, want) {
		t.Fatalf("host handed protocol %#v, want the decoded JSON untouched", adapter.applied)
	}
	// 协议层返回的 uploads 环境必须原样进入 BuildCreate，宿主不加键也不删键。
	if len(adapter.created) != 1 {
		t.Fatalf("BuildCreate calls = %d", len(adapter.created))
	}
	if !reflect.DeepEqual(adapter.created[0].Uploads, adapter.captured) {
		t.Fatalf("create uploads = %#v, want %#v", adapter.created[0].Uploads, adapter.captured)
	}
	if adapter.created[0].BaseURL != server.URL || adapter.created[0].Request.Model != "probe-model" {
		t.Fatalf("create context = %#v", adapter.created[0])
	}
}

func TestDeclarativeUploadRequestsUseHostOutboundBoundary(t *testing.T) {
	adapter := uploadsProbeAdapter(t, uploadsProbeManifestWithUploads)
	t.Run("private upstream refused before create", func(t *testing.T) {
		t.Setenv("CANVAS_ALLOW_PRIVATE_UPSTREAMS", "false")
		t.Setenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS", "")
		probe := newUploadsProbe(t, uploadsProbeOptions{})
		_, err := RunProtocolAdapterTask(WithRuntime(context.Background(), taskRuntime()), uploadsProbeInput(probe.server.URL, 1), adapter)
		if err == nil {
			t.Fatal("SSRF policy must stop the declared upload, not silently create")
		}
		if log, _, _, _ := probe.snapshot(); len(log) != 0 {
			t.Fatalf("refused upload still reached the upstream: %v", log)
		}
	})
	t.Run("credentials receipts and stage come from the host", func(t *testing.T) {
		t.Setenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS", "127.0.0.1")
		probe := newUploadsProbe(t, uploadsProbeOptions{})
		receipts := &recordingReceipts{}
		stages := &recordingStages{}
		ctx := WithRuntime(context.Background(), taskRuntime(func(runtime *Runtime) {
			runtime.Receipts = receipts
			runtime.Stages = stages
		}))
		if _, err := RunProtocolAdapterTask(ctx, uploadsProbeInput(probe.server.URL, 1), adapter); err != nil {
			t.Fatal(err)
		}
		if got := probe.authFor("/upload/image"); got != "Bearer probe-key" {
			t.Fatalf("upload auth = %q, want the channel credential injected by the host", got)
		}
		var observed int
		for _, observation := range receipts.snapshot() {
			if observation.Request != nil && strings.HasSuffix(observation.Request.URL.Path, "/upload/image") {
				observed++
			}
		}
		if observed != 1 {
			t.Fatalf("upload receipts = %d, want the shared transport to observe it", observed)
		}
		steps := stages.snapshot()
		if !slices.Contains(steps, "正在上传参考素材") {
			t.Fatalf("stages = %v, want the upload phase announced", steps)
		}
	})
}

type recordingStages struct {
	mu    sync.Mutex
	steps []string
}

func (s *recordingStages) SetStage(_ context.Context, stage string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, stage)
	return nil
}

func (s *recordingStages) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.steps...)
}

// plainProbeAdapter 是宿主绑定的执行引擎形状：没有实现 UploadAdapter，
// 预上传接缝必须整段跳过，而不是发出一次空上传。
type plainProbeAdapter struct{}

func (plainProbeAdapter) Metadata() protocol.Metadata {
	return protocol.Metadata{ID: "uploads-host-plain", Enabled: true, Categories: []protocol.Capability{protocol.CapabilityText}}
}

func (plainProbeAdapter) BuildCreate(context.Context, protocol.RequestContext) (protocol.RequestSpec, error) {
	return protocol.RequestSpec{Method: "POST", Path: "/prompt", ContentType: "application/json", Body: map[string]any{"prompt": "draw"}}, nil
}

func (plainProbeAdapter) ParseCreate(context.Context, []byte) (protocol.CreateResult, error) {
	return succeededTextResult(), nil
}

func (plainProbeAdapter) BuildPoll(context.Context, protocol.PollContext) (protocol.RequestSpec, error) {
	return protocol.RequestSpec{Method: "GET", Path: "/history"}, nil
}

func (plainProbeAdapter) ParsePoll(context.Context, protocol.PollContext, []byte) (protocol.PollResult, error) {
	return protocol.PollResult{Status: protocol.StatusSucceeded, Result: textResult("polled")}, nil
}

func (plainProbeAdapter) BuildCancel(context.Context, protocol.PollContext) (protocol.RequestSpec, error) {
	return protocol.RequestSpec{}, fmt.Errorf("plain probe adapter declares no cancel")
}

// recordingUploadAdapter 是协议层的替身：记录宿主实际交回的回执，用来证明宿主只搬运
// 解码后的 JSON 对象，并且只调用一次 ApplyUploadResults 就把全部步骤结果送回去。
type recordingUploadAdapter struct {
	steps []protocol.ManifestUploadStep
	plainProbeAdapter
	mu         sync.Mutex
	applied    []protocol.ManifestUploadResult
	captured   map[string]any
	applyCalls int
	created    []protocol.RequestContext
}

func (a *recordingUploadAdapter) Metadata() protocol.Metadata {
	return protocol.Metadata{ID: "uploads-host-recording", Enabled: true, Categories: []protocol.Capability{protocol.CapabilityText}}
}

func (a *recordingUploadAdapter) BuildUploadRequests(context.Context, protocol.UploadContext) ([]protocol.ManifestUploadStep, error) {
	return a.steps, nil
}

func (a *recordingUploadAdapter) ApplyUploadResults(_ context.Context, c protocol.RequestContext, results []protocol.ManifestUploadResult) (protocol.RequestContext, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.applyCalls++
	a.applied = append(a.applied, results...)
	c.Uploads = a.captured
	return c, nil
}

func (a *recordingUploadAdapter) BuildCreate(_ context.Context, c protocol.RequestContext) (protocol.RequestSpec, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.created = append(a.created, c)
	return protocol.RequestSpec{Method: "POST", Path: "/prompt", ContentType: "application/json", Body: map[string]any{"prompt": "draw"}}, nil
}

func (a *recordingUploadAdapter) ParseCreate(context.Context, []byte) (protocol.CreateResult, error) {
	return succeededTextResult(), nil
}

func succeededTextResult() protocol.CreateResult {
	return protocol.CreateResult{TaskID: "pid-1", Status: protocol.StatusSucceeded, Result: textResult("created")}
}

func textResult(text string) *protocol.Result {
	return &protocol.Result{Text: text}
}
