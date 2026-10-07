package protocol

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

type ManifestOperation struct {
	Method              string             `json:"method"`
	Path                string             `json:"path"`
	PathTemplate        any                `json:"pathTemplate,omitempty"`
	OriginPath          bool               `json:"originPath,omitempty"`
	ContentType         string             `json:"contentType,omitempty"`
	ContentTypeTemplate any                `json:"contentTypeTemplate,omitempty"`
	Headers             map[string]any     `json:"headers,omitempty"`
	Query               map[string]any     `json:"query,omitempty"`
	Body                any                `json:"body,omitempty"`
	Fields              map[string]string  `json:"fields,omitempty"`
	Files               []ManifestFilePart `json:"files,omitempty"`
	// Policy is read only on the poll operation and declares the upstream's own
	// suggested cadence. It is carried here because poll shares the operation
	// shape with create, cancel and result; those ignore it.
	Policy *PollPolicy `json:"policy,omitempty"`
}

// ManifestUpload declares an upload that runs before create. "foreach" is an
// optional expression that must resolve to an array; the step executes once per
// item with the current element bound as "item" and its position as "index", and
// the captured values are collected as an array aligned with that input order.
// Without "foreach" the step runs exactly once and captures a single object.
//
// "capture" names the fields taken from the decoded upload response. The mapping
// is evaluated inside the protocol, so the host never sees an expression.
type ManifestUpload struct {
	ID        string            `json:"id"`
	Foreach   any               `json:"foreach,omitempty"`
	Operation ManifestOperation `json:"operation"`
	Capture   map[string]any    `json:"capture,omitempty"`
}

type ManifestFilePart struct {
	Name     string `json:"name"`
	Source   any    `json:"source"`
	Filename any    `json:"filename,omitempty"`
	MIMEType any    `json:"mimeType,omitempty"`
}

type ManifestResponse struct {
	TaskIDPaths     []string `json:"taskIdPaths,omitempty"`
	StatusPaths     []string `json:"statusPaths,omitempty"`
	ErrorPaths      []string `json:"errorPaths,omitempty"`
	MessagePaths    []string `json:"messagePaths,omitempty"`
	TextPaths       []string `json:"textPaths,omitempty"`
	ReasoningPaths  []string `json:"reasoningPaths,omitempty"`
	ResultURLPaths  []string `json:"resultUrlPaths,omitempty"`
	ResultPaths     []string `json:"resultPaths,omitempty"`
	ResultKind      string   `json:"resultKind,omitempty"`
	ResultEphemeral bool     `json:"resultEphemeral,omitempty"`
	// BinaryPayload 表示 create 同步返回二进制媒体（如 /audio/speech 的音频流）。
	// 声明式解析会把整个响应体包装为对应能力的单个媒体结果，不做 JSON 路径提取。
	BinaryPayload bool `json:"binaryPayload,omitempty"`
	TaskID        any  `json:"taskId,omitempty"`
	Status        any  `json:"status,omitempty"`
	Message       any  `json:"message,omitempty"`
	Text          any  `json:"text,omitempty"`
	Reasoning     any  `json:"reasoning,omitempty"`
	Images        any  `json:"images,omitempty"`
	Videos        any  `json:"videos,omitempty"`
	Audios        any  `json:"audios,omitempty"`
	Usage         any  `json:"usage,omitempty"`
}

// ManifestAgentResponse describes the provider response shape for a
// tool-capable text request. Tool call paths are resolved against each item in
// ToolCallsPath.
type ManifestAgentResponse struct {
	TextPaths              []string `json:"textPaths,omitempty"`
	ReasoningPaths         []string `json:"reasoningPaths,omitempty"`
	ToolCallsPath          string   `json:"toolCallsPath,omitempty"`
	ToolCallIDPaths        []string `json:"toolCallIdPaths,omitempty"`
	ToolCallNamePaths      []string `json:"toolCallNamePaths,omitempty"`
	ToolCallArgsPaths      []string `json:"toolCallArgumentsPaths,omitempty"`
	ToolCallSignaturePaths []string `json:"toolCallThoughtSignaturePaths,omitempty"`
}

// AdapterResolver is used by the host to bind a shipped execution engine to a
// manifest. Uploaded plugins must use the declarative path; host bindings are
// reserved for manifests shipped with the application.
type AdapterResolver func(string) (Adapter, bool)

func LoadInstalledPlugin(data []byte, resolve AdapterResolver) (Adapter, error) {
	adapters, err := LoadInstalledProviders(data, resolve)
	if err != nil {
		return nil, err
	}
	if len(adapters) == 0 {
		return nil, fmt.Errorf("plugin does not provide an executable provider")
	}
	return adapters[0], nil
}

// LoadInstalledProviders loads every provider contribution from one plugin
// package. The package remains the lifecycle and permission unit; providers
// are separate execution entries in the runtime index.
func LoadInstalledProviders(data []byte, resolve AdapterResolver) ([]Adapter, error) {
	manifest, err := decodeManifest(data)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(strings.TrimSpace(manifest.Runtime.Backend), "host:") {
		if len(manifest.Contributes.Providers) != 1 {
			return nil, fmt.Errorf("host-backed plugin must declare exactly one provider")
		}
		if resolve == nil {
			return nil, fmt.Errorf("plugin %q requires a host execution engine", manifest.Metadata.ID)
		}
		name := strings.TrimPrefix(strings.TrimSpace(manifest.Runtime.Backend), "host:")
		adapter, ok := resolve(name)
		if !ok {
			return nil, fmt.Errorf("plugin host execution %q is unavailable", name)
		}
		if err := ValidateManifest(manifest); err != nil {
			return nil, err
		}
		if err := normalizeManifest(&manifest); err != nil {
			return nil, err
		}
		return []Adapter{metadataAdapter{metadata: manifest.Metadata, delegate: adapter}}, nil
	}
	if len(manifest.Contributes.Providers) == 0 {
		adapter, err := loadDeclarativeManifest(manifest)
		if err != nil {
			return nil, err
		}
		return []Adapter{adapter}, nil
	}
	result := make([]Adapter, 0, len(manifest.Contributes.Providers))
	for index := range manifest.Contributes.Providers {
		adapter, err := loadDeclarativeManifestProvider(manifest, index)
		if err != nil {
			return nil, err
		}
		result = append(result, adapter)
	}
	return result, nil
}

func validatePluginMetadata(metadata Metadata) error {
	if strings.TrimSpace(metadata.ID) == "" || strings.TrimSpace(metadata.Version) == "" || !validManifestIdentifier(metadata.ID) {
		return fmt.Errorf("protocol plugin metadata is invalid")
	}
	if len(metadata.Categories) == 0 || len(metadata.Scopes) == 0 {
		return fmt.Errorf("protocol plugin metadata requires categories and scopes")
	}
	for _, capability := range metadata.Categories {
		if capability != CapabilityText && capability != CapabilityImage && capability != CapabilityVideo && capability != CapabilityAudio {
			return fmt.Errorf("unsupported protocol capability %q", capability)
		}
	}
	for _, scope := range metadata.Scopes {
		switch scope {
		case SurfaceAdminSystemChannel, SurfaceUserCustomChannel, SurfaceCanvas, SurfaceCreation, SurfaceAgent:
		default:
			return fmt.Errorf("unsupported protocol scope %q", scope)
		}
	}
	return nil
}

type metadataAdapter struct {
	metadata Metadata
	delegate Adapter
}

func (a metadataAdapter) Metadata() Metadata { return a.metadata }
func (a metadataAdapter) AgentAvailable() bool {
	capability, ok := a.delegate.(AgentCapability)
	return ok && capability.AgentAvailable()
}
func (a metadataAdapter) ResultAvailable() bool {
	capability, ok := a.delegate.(ResultCapability)
	return ok && capability.ResultAvailable()
}
func (a metadataAdapter) BuildCreate(ctx context.Context, c RequestContext) (RequestSpec, error) {
	return a.delegate.BuildCreate(ctx, c)
}
func (a metadataAdapter) ParseCreate(ctx context.Context, body []byte) (CreateResult, error) {
	return a.delegate.ParseCreate(ctx, body)
}
func (a metadataAdapter) BuildPoll(ctx context.Context, c PollContext) (RequestSpec, error) {
	return a.delegate.BuildPoll(ctx, c)
}
func (a metadataAdapter) ParsePoll(ctx context.Context, c PollContext, body []byte) (PollResult, error) {
	return a.delegate.ParsePoll(ctx, c, body)
}
func (a metadataAdapter) BuildCancel(ctx context.Context, c PollContext) (RequestSpec, error) {
	return a.delegate.BuildCancel(ctx, c)
}

func (a metadataAdapter) BuildCancelRequests(ctx context.Context, c PollContext) ([]RequestSpec, error) {
	adapter, ok := a.delegate.(CancelRequestsAdapter)
	if !ok {
		spec, err := a.delegate.BuildCancel(ctx, c)
		if err != nil {
			return nil, err
		}
		return []RequestSpec{spec}, nil
	}
	return adapter.BuildCancelRequests(ctx, c)
}
func (a metadataAdapter) BuildResult(ctx context.Context, c PollContext) (RequestSpec, error) {
	adapter, ok := a.delegate.(ResultAdapter)
	if !ok {
		return RequestSpec{}, unavailable(a.metadata)
	}
	return adapter.BuildResult(ctx, c)
}
func (a metadataAdapter) BuildAgent(ctx context.Context, c AgentRequestContext) (RequestSpec, error) {
	adapter, ok := a.delegate.(AgentAdapter)
	if !ok {
		return RequestSpec{}, unavailable(a.metadata)
	}
	return adapter.BuildAgent(ctx, c)
}

// BuildUploadRequests / ApplyUploadResults 与 BuildCancelRequests 同构：宿主绑定的
// 执行引擎自己声明预上传时才转发，否则回退成"无 uploads 声明"——空步骤、原样 context。
func (a metadataAdapter) BuildUploadRequests(ctx context.Context, c UploadContext) ([]ManifestUploadStep, error) {
	adapter, ok := a.delegate.(UploadAdapter)
	if !ok {
		return nil, nil
	}
	return adapter.BuildUploadRequests(ctx, c)
}

func (a metadataAdapter) ApplyUploadResults(ctx context.Context, c RequestContext, results []ManifestUploadResult) (RequestContext, error) {
	adapter, ok := a.delegate.(UploadAdapter)
	if !ok {
		return c, nil
	}
	return adapter.ApplyUploadResults(ctx, c, results)
}

func (a metadataAdapter) PollPolicy() (PollPolicy, bool) {
	adapter, ok := a.delegate.(PollPolicyAdapter)
	if !ok {
		return PollPolicy{}, false
	}
	return adapter.PollPolicy()
}
func (a metadataAdapter) ParseAgent(ctx context.Context, body []byte) (AgentResult, error) {
	adapter, ok := a.delegate.(AgentAdapter)
	if !ok {
		return AgentResult{}, unavailable(a.metadata)
	}
	return adapter.ParseAgent(ctx, body)
}

func LoadManifest(data []byte) (Adapter, error) {
	manifest, err := decodeManifest(data)
	if err != nil {
		return nil, err
	}
	return loadDeclarativeManifest(manifest)
}

func decodeManifest(data []byte) (Manifest, error) {
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode plugin manifest: %w", err)
	}
	if err := ValidateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func loadDeclarativeManifest(manifest Manifest) (Adapter, error) {
	manifest.Runtime.Backend = "declarative"
	if err := normalizeManifest(&manifest); err != nil {
		return nil, err
	}
	return manifestAdapter{manifest: manifest}, nil
}

func loadDeclarativeManifestProvider(manifest Manifest, index int) (Adapter, error) {
	manifest.Runtime.Backend = "declarative"
	if err := normalizeManifestForProvider(&manifest, index); err != nil {
		return nil, err
	}
	return manifestAdapter{manifest: manifest}, nil
}

func ValidateManifest(manifest Manifest) error {
	if version := strings.TrimSpace(manifest.APIVersion); version != "beeftv.plugin/v1" && version != "beeftv.plugin/v2" {
		return fmt.Errorf("unsupported protocol manifest apiVersion %q", manifest.APIVersion)
	}
	if strings.TrimSpace(manifest.Metadata.ID) == "" || strings.TrimSpace(manifest.Metadata.Version) == "" {
		return fmt.Errorf("protocol manifest metadata requires id and version")
	}
	if strings.TrimSpace(manifest.Metadata.Name) == "" {
		return fmt.Errorf("protocol manifest metadata requires name")
	}
	if !validManifestIdentifier(manifest.Metadata.ID) {
		return fmt.Errorf("protocol manifest metadata id is invalid")
	}
	if len(manifest.Metadata.Name) > 160 || len(manifest.Metadata.Vendor) > 120 {
		return fmt.Errorf("protocol manifest metadata name or vendor is too long")
	}
	if len(manifest.Contributes.Providers) == 0 && !hasNonProviderContribution(manifest.Contributes) {
		return fmt.Errorf("plugin must declare at least one contribution")
	}
	if backend := strings.TrimSpace(manifest.Runtime.Backend); backend != "" && backend != "declarative" && backend != "rpc" && backend != "wasm" && backend != "trusted-backend" && !strings.HasPrefix(backend, "host:") {
		return fmt.Errorf("unsupported plugin backend %q", backend)
	}
	if len(manifest.Contributes.Providers) == 0 {
		return nil
	}
	providerIDs := make(map[string]struct{}, len(manifest.Contributes.Providers))
	for index, provider := range manifest.Contributes.Providers {
		if strings.TrimSpace(provider.ID) == "" || strings.TrimSpace(provider.Label) == "" || !validManifestIdentifier(provider.ID) {
			return fmt.Errorf("provider contribution %d requires a valid id and label", index)
		}
		if _, exists := providerIDs[provider.ID]; exists {
			return fmt.Errorf("duplicate provider contribution %q", provider.ID)
		}
		providerIDs[provider.ID] = struct{}{}
		if len(provider.Capabilities) == 0 || len(provider.Scopes) == 0 {
			return fmt.Errorf("provider contribution %q requires capabilities and scopes", provider.ID)
		}
		for _, capability := range provider.Capabilities {
			if capability != CapabilityText && capability != CapabilityImage && capability != CapabilityVideo && capability != CapabilityAudio {
				return fmt.Errorf("unsupported protocol capability %q", capability)
			}
		}
		for _, scope := range provider.Scopes {
			switch scope {
			case SurfaceAdminSystemChannel, SurfaceUserCustomChannel, SurfaceCanvas, SurfaceCreation, SurfaceAgent:
			default:
				return fmt.Errorf("unsupported protocol scope %q", scope)
			}
		}
		if err := validateManifestOperation(provider.Create); err != nil {
			return fmt.Errorf("provider %q create operation: %w", provider.ID, err)
		}
		if err := validateManifestUploads(provider); err != nil {
			return err
		}
		if err := validateManifestPollPolicy(provider.ID, provider.Poll); err != nil {
			return err
		}
		if provider.Agent != nil {
			if err := validateManifestOperation(*provider.Agent); err != nil {
				return fmt.Errorf("provider %q agent operation: %w", provider.ID, err)
			}
			if provider.AgentResponse == nil {
				return fmt.Errorf("provider %q agent response mapping is required when agent operation is declared", provider.ID)
			}
		}
		if provider.Poll != nil {
			if err := validateManifestOperation(*provider.Poll); err != nil {
				return fmt.Errorf("provider %q poll operation: %w", provider.ID, err)
			}
		}
		if provider.Cancel != nil {
			if err := validateManifestOperation(*provider.Cancel); err != nil {
				return fmt.Errorf("provider %q cancel operation: %w", provider.ID, err)
			}
		}
		if len(provider.CancelOperations) > 0 && provider.Cancel == nil {
			return fmt.Errorf("provider %q cancelOperations requires a cancel operation", provider.ID)
		}
		for index, operation := range provider.CancelOperations {
			if operation == nil {
				return fmt.Errorf("provider %q cancel operation %d is missing", provider.ID, index)
			}
			if err := validateManifestOperation(*operation); err != nil {
				return fmt.Errorf("provider %q cancel operation %d: %w", provider.ID, index, err)
			}
		}
		if provider.Result != nil {
			if err := validateManifestOperation(*provider.Result); err != nil {
				return fmt.Errorf("provider %q result operation: %w", provider.ID, err)
			}
		}
		for ruleIndex, rule := range provider.Validations {
			if rule.Assert == nil || strings.TrimSpace(rule.Message) == "" {
				return fmt.Errorf("provider %q validation %d requires assert and message", provider.ID, ruleIndex)
			}
		}
	}
	return nil
}

func normalizeManifest(manifest *Manifest) error {
	if manifest == nil {
		return fmt.Errorf("plugin manifest is missing")
	}
	if len(manifest.Contributes.Providers) == 0 {
		manifest.Metadata.Execution = manifest.Runtime.Backend
		return nil
	}
	return normalizeManifestForProvider(manifest, 0)
}

func normalizeManifestForProvider(manifest *Manifest, index int) error {
	if manifest == nil || index < 0 || index >= len(manifest.Contributes.Providers) {
		return fmt.Errorf("plugin provider contribution is missing")
	}
	provider := manifest.Contributes.Providers[index]
	// Released enterprise profiles used the package name as the protocol ID.
	// Keep those profiles on this installed adapter, including its disabled state.
	if manifest.Metadata.ID == "openai-videos" && provider.ID == "newapi" {
		manifest.Metadata.LegacyAliases = []string{"openai-video", "openai-videos"}
	}
	manifest.Metadata.ID = provider.ID
	manifest.Metadata.Categories = provider.Capabilities
	manifest.Metadata.Scopes = provider.Scopes
	manifest.Metadata.Parameters = provider.Parameters
	manifest.Metadata.Create = operationSummary(provider.Create)
	manifest.Metadata.Poll = operationSummaryPtr(provider.Poll)
	manifest.Metadata.Cancel = operationSummaryPtr(provider.Cancel)
	manifest.Metadata.ContentType = provider.Create.ContentType
	manifest.Metadata.RequiresPublicMediaURLs = provider.RequiresPublicMediaURLs
	manifest.Metadata.Execution = manifest.Runtime.Backend
	manifest.Create = provider.Create
	manifest.Agent = provider.Agent
	manifest.Poll = provider.Poll
	manifest.Cancel = provider.Cancel
	manifest.CancelOperations = provider.CancelOperations
	manifest.ResultOperation = provider.Result
	manifest.Uploads = provider.Uploads
	manifest.Response = provider.Response
	manifest.AgentResponse = provider.AgentResponse
	manifest.Auth = provider.Auth
	manifest.Validations = provider.Validations
	return nil
}

func hasNonProviderContribution(contributes ManifestContributions) bool {
	return len(contributes.Workflows) > 0 || len(contributes.CanvasNodes) > 0 || len(contributes.Transforms) > 0 || len(contributes.Commands) > 0 || len(contributes.AssetSources) > 0 || len(contributes.UsageObservers) > 0 || len(contributes.AICapabilities) > 0 || len(contributes.Agents) > 0 || len(contributes.ImportExport) > 0
}

func operationSummary(operation ManifestOperation) string {
	path := strings.ReplaceAll(operation.Path, "{{model}}", "{model}")
	path = strings.ReplaceAll(path, "{{taskId}}", "{task_id}")
	return strings.ToUpper(operation.Method) + " " + path
}

func operationSummaryPtr(operation *ManifestOperation) string {
	if operation == nil {
		return ""
	}
	return operationSummary(*operation)
}

func validateManifestOperation(operation ManifestOperation) error {
	method := strings.ToUpper(strings.TrimSpace(operation.Method))
	if method != http.MethodGet && method != http.MethodPost && method != http.MethodDelete && method != http.MethodPut {
		return fmt.Errorf("unsupported HTTP method %q", operation.Method)
	}
	if operation.PathTemplate == nil && !isRelativePath(operation.Path) {
		return fmt.Errorf("path must be relative: %q", operation.Path)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(defaultValue(operation.ContentType, "application/json"), ";")[0]))
	switch contentType {
	case "application/json", "multipart/form-data", "application/x-www-form-urlencoded", "application/octet-stream", "":
	default:
		return fmt.Errorf("unsupported content type %q", operation.ContentType)
	}
	if len(operation.Files) > 0 && operation.ContentTypeTemplate == nil && contentType != "multipart/form-data" {
		return fmt.Errorf("file parts require multipart/form-data")
	}
	for _, file := range operation.Files {
		if strings.TrimSpace(file.Name) == "" || file.Source == nil {
			return fmt.Errorf("multipart file part requires name and source")
		}
	}
	return nil
}

// validateManifestUploads keeps the pre-create upload pass addressable: every
// step needs a unique id because that id is the only handle create expressions
// have on the captured values, and its operation must be a real outbound
// request the host can execute.
func validateManifestUploads(provider ManifestProvider) error {
	seen := make(map[string]struct{}, len(provider.Uploads))
	for index, upload := range provider.Uploads {
		id := strings.TrimSpace(upload.ID)
		if !validManifestUploadIdentifier(id) {
			return fmt.Errorf("provider %q upload %d requires a usable id", provider.ID, index)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("duplicate upload id %q for provider %q", id, provider.ID)
		}
		seen[id] = struct{}{}
		if err := validateManifestOperation(upload.Operation); err != nil {
			return fmt.Errorf("provider %q upload %q operation: %w", provider.ID, id, err)
		}
	}
	return nil
}

// validManifestUploadIdentifier keeps an upload addressable rather than naming
// it like a provider: captures are read as "$ref": "uploads.<id>.<index>", so a
// dot or a space would break that path and a "$" would make the id read as an
// operator node. camelCase ids such as "referenceImage" stay valid, which the
// stricter provider identifier rule would not allow.
func validManifestUploadIdentifier(value string) bool {
	if value == "" || len(value) > 96 {
		return false
	}
	for _, char := range value {
		if char == '.' || char == '$' || char == '[' || char == ']' || unicode.IsSpace(char) {
			return false
		}
	}
	return true
}

// validateManifestPollPolicy range-checks the declared cadence without giving
// the protocol an opinion on what the host should use when a knob is absent.
// Zero is the absent value on this wire type, so an omitted knob never fails.
func validateManifestPollPolicy(providerID string, poll *ManifestOperation) error {
	if poll == nil || poll.Policy == nil {
		return nil
	}
	policy := poll.Policy
	if policy.IntervalSeconds < 0 || policy.IntervalSeconds > 300 {
		return fmt.Errorf("provider %q poll policy intervalSeconds must be between 1 and 300", providerID)
	}
	if policy.TimeoutSeconds < 0 {
		return fmt.Errorf("provider %q poll policy timeoutSeconds must not be negative", providerID)
	}
	if policy.TimeoutSeconds > 0 && policy.TimeoutSeconds <= policy.IntervalSeconds {
		return fmt.Errorf("provider %q poll policy timeoutSeconds must be greater than intervalSeconds", providerID)
	}
	return nil
}

type manifestAdapter struct{ manifest Manifest }

func (a manifestAdapter) Metadata() Metadata { return a.manifest.Metadata }
func (a manifestAdapter) AgentAvailable() bool {
	return a.manifest.Agent != nil && a.manifest.AgentResponse != nil
}
func (a manifestAdapter) ResultAvailable() bool { return a.manifest.ResultOperation != nil }
func (a manifestAdapter) BuildCreate(_ context.Context, c RequestContext) (RequestSpec, error) {
	if a.manifest.Metadata.ID == "volcengine-ark-seedance" || a.manifest.Metadata.ID == "volcengine-ark-video" || a.manifest.Metadata.ID == "volcengine-ark-agent-plan-video" || a.manifest.Metadata.ID == "seedance-videos-compatible" {
		c.Request = NormalizeSeedanceTaskOptions(c.Request)
	}
	if len(a.manifest.Contributes.Providers) == 0 {
		return RequestSpec{}, fmt.Errorf("plugin %s does not provide a provider", a.manifest.Metadata.ID)
	}
	if err := validateManifestRequest(a.manifest.Validations, c.Request); err != nil {
		return RequestSpec{}, err
	}
	spec, err := buildManifestOperation(a.manifest.Create, a.manifest.Auth, manifestOperationInput{Request: c.Request, Uploads: c.Uploads})
	if err != nil {
		return RequestSpec{}, err
	}
	if a.manifest.Metadata.ID == "volcengine-ark-seedance" || a.manifest.Metadata.ID == "volcengine-ark-video" || a.manifest.Metadata.ID == "volcengine-ark-agent-plan-video" || a.manifest.Metadata.ID == "seedance-videos-compatible" {
		if kind := SeedanceOmniTaskType(c.Request); kind != "" {
			if body, ok := spec.Body.(map[string]any); ok {
				body["omni_reference_task_type"] = kind
			}
		}
	}
	// Some NewAPI-compatible gateways validate the prompt under input.content
	// even though the historical channel-1 contract used input.prompt. Keep the
	// legacy field and add the canonical content alias for that shipped profile.
	if a.manifest.Metadata.ID == "newapi-channel-1" && c.Request.Capability == CapabilityVideo {
		if body, ok := spec.Body.(map[string]any); ok {
			if input, ok := body["input"].(map[string]any); ok {
				if _, exists := input["content"]; !exists {
					if prompt, ok := input["prompt"]; ok && strings.TrimSpace(fmt.Sprint(prompt)) != "" {
						input["content"] = []any{map[string]any{"type": "text", "text": fmt.Sprint(prompt)}}
					}
				}
				if content, ok := input["content"]; ok {
					// WhatsToken's gateway validates the ByteDance-compatible
					// content array at the top level, while older NewAPI channels
					// keep it under input. Send both projections during migration.
					if _, exists := body["content"]; !exists {
						body["content"] = content
					}
				}
			}
		}
	}
	return spec, nil
}
func (a manifestAdapter) BuildAgent(_ context.Context, c AgentRequestContext) (RequestSpec, error) {
	if a.manifest.Agent == nil {
		return RequestSpec{}, fmt.Errorf("protocol %s has no agent operation", a.manifest.Metadata.ID)
	}
	request := GenerationRequest{Model: c.Model, Extra: map[string]any{"agent": c.Request}}
	return buildManifestOperation(*a.manifest.Agent, a.manifest.Auth, manifestOperationInput{Request: request})
}
func (a manifestAdapter) ParseAgent(_ context.Context, body []byte) (AgentResult, error) {
	if a.manifest.AgentResponse == nil {
		return AgentResult{}, fmt.Errorf("protocol %s has no agent response mapping", a.manifest.Metadata.ID)
	}
	payload, err := decodeObject(body)
	if err != nil {
		return AgentResult{}, err
	}
	response := a.manifest.AgentResponse
	result := AgentResult{Text: firstPathValue(payload, response.TextPaths...), Reasoning: firstPathValue(payload, response.ReasoningPaths...)}
	if response.ToolCallsPath == "" {
		return result, nil
	}
	for index, item := range arrayValue(pathValue(payload, response.ToolCallsPath)) {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		call := AgentToolCall{
			ID:               firstPathValue(object, response.ToolCallIDPaths...),
			Name:             firstPathValue(object, response.ToolCallNamePaths...),
			Arguments:        firstPathJSONValue(object, response.ToolCallArgsPaths...),
			ThoughtSignature: firstPathValue(object, response.ToolCallSignaturePaths...),
		}
		if strings.TrimSpace(call.Name) != "" {
			if strings.TrimSpace(call.ID) == "" {
				call.ID = syntheticAgentToolCallID(body, index)
			}
			result.ToolCalls = append(result.ToolCalls, call)
		}
	}
	return result, nil
}

func syntheticAgentToolCallID(body []byte, index int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s#%d", body, index)))
	return fmt.Sprintf("call_%x", sum[:8])
}
func (a manifestAdapter) ParseCreate(_ context.Context, body []byte) (CreateResult, error) {
	if a.manifest.Response.BinaryPayload {
		return binaryPayloadCreateResult(a.manifest.Response.ResultKind, body)
	}
	payload, err := decodeObject(body)
	if err != nil {
		return CreateResult{}, err
	}
	return a.parse(payload, PollContext{}), nil
}

// binaryPayloadCreateResult 把同步二进制响应包装为单个媒体结果。MIME 以响应内容探测为准，
// 空响应必须失败，不能把空内容伪装成生成成功。
func binaryPayloadCreateResult(resultKind string, body []byte) (CreateResult, error) {
	if len(body) == 0 {
		return CreateResult{}, fmt.Errorf("binary payload response is empty")
	}
	mimeType := strings.ToLower(strings.TrimSpace(strings.Split(http.DetectContentType(body), ";")[0]))
	reference := MediaReference{DataURL: "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(body), MIMEType: mimeType}
	result := &Result{}
	switch resultKind {
	case "audio":
		result.Audios = []MediaReference{reference}
	case "image":
		result.Images = []MediaReference{reference}
	case "video":
		result.Videos = []MediaReference{reference}
	default:
		return CreateResult{}, fmt.Errorf("binary payload response requires resultKind image, video or audio, got %q", resultKind)
	}
	return CreateResult{Status: StatusSucceeded, Result: result}, nil
}
func (a manifestAdapter) BuildPoll(_ context.Context, c PollContext) (RequestSpec, error) {
	if a.manifest.Poll == nil {
		return RequestSpec{}, fmt.Errorf("protocol %s has no poll operation", a.manifest.Metadata.ID)
	}
	request := c.Request
	request.Model = c.Model
	return buildManifestOperation(*a.manifest.Poll, a.manifest.Auth, manifestOperationInput{Request: request, TaskID: c.TaskID, Uploads: c.Uploads})
}
func (a manifestAdapter) ParsePoll(_ context.Context, c PollContext, body []byte) (PollResult, error) {
	payload, err := decodeObject(body)
	if err != nil {
		return PollResult{}, err
	}
	result := a.parse(payload, c)
	return PollResult{TaskID: result.TaskID, Status: result.Status, Result: result.Result, Message: result.Message}, nil
}
func (a manifestAdapter) BuildCancel(_ context.Context, c PollContext) (RequestSpec, error) {
	if a.manifest.Cancel == nil {
		return RequestSpec{}, fmt.Errorf("protocol %s does not support cancellation", a.manifest.Metadata.ID)
	}
	request := c.Request
	request.Model = c.Model
	return buildManifestOperation(*a.manifest.Cancel, a.manifest.Auth, manifestOperationInput{Request: request, TaskID: c.TaskID, Uploads: c.Uploads})
}

// BuildCancelRequests 按声明顺序返回 cancel 与其补发请求。取消要覆盖排队与执行两种
// 状态的协议（例如 ComfyUI 的 /queue delete 加 /interrupt）靠这一个端口表达，
// 凭证注入、SSRF 校验与出站边界仍由宿主执行。
func (a manifestAdapter) BuildCancelRequests(_ context.Context, c PollContext) ([]RequestSpec, error) {
	if a.manifest.Cancel == nil {
		return nil, fmt.Errorf("protocol %s does not support cancellation", a.manifest.Metadata.ID)
	}
	request := c.Request
	request.Model = c.Model
	specs := make([]RequestSpec, 0, 1+len(a.manifest.CancelOperations))
	primary, err := buildManifestOperation(*a.manifest.Cancel, a.manifest.Auth, manifestOperationInput{Request: request, TaskID: c.TaskID, Uploads: c.Uploads})
	if err != nil {
		return nil, err
	}
	specs = append(specs, primary)
	for index, operation := range a.manifest.CancelOperations {
		if operation == nil {
			continue
		}
		spec, err := buildManifestOperation(*operation, a.manifest.Auth, manifestOperationInput{Request: request, TaskID: c.TaskID, Uploads: c.Uploads})
		if err != nil {
			return nil, fmt.Errorf("cancel operation %d: %w", index, err)
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

func (a manifestAdapter) BuildResult(_ context.Context, c PollContext) (RequestSpec, error) {
	if a.manifest.ResultOperation == nil {
		return RequestSpec{}, fmt.Errorf("protocol %s has no result operation", a.manifest.Metadata.ID)
	}
	request := c.Request
	request.Model = c.Model
	return buildManifestOperation(*a.manifest.ResultOperation, a.manifest.Auth, manifestOperationInput{Request: request, TaskID: c.TaskID, Uploads: c.Uploads})
}

// PollPolicy 暴露 poll.policy 声明的节奏。未声明时第二个返回值为 false：协议层不造
// 默认值，宿主继续用它自己的外层调度（租约、not-found 预算这些本层不表达的语义）。
func (a manifestAdapter) PollPolicy() (PollPolicy, bool) {
	if a.manifest.Poll == nil || a.manifest.Poll.Policy == nil {
		return PollPolicy{}, false
	}
	return *a.manifest.Poll.Policy, true
}

// manifestUploadIteration is one concrete upload the host has to perform: a
// declared step bound to a single foreach element. BuildUploadRequests and
// ApplyUploadResults replay the same plan, so a capture is always applied to the
// exact item and index its request was built for.
type manifestUploadIteration struct {
	id        string
	index     int
	foreach   bool
	item      any
	operation ManifestOperation
	capture   map[string]any
	request   GenerationRequest
}

func (a manifestAdapter) uploadPlan(request GenerationRequest) ([]manifestUploadIteration, error) {
	if len(a.manifest.Uploads) == 0 {
		return nil, nil
	}
	plan := make([]manifestUploadIteration, 0, len(a.manifest.Uploads))
	for _, upload := range a.manifest.Uploads {
		id := strings.TrimSpace(upload.ID)
		if upload.Foreach == nil {
			plan = append(plan, manifestUploadIteration{id: id, operation: upload.Operation, capture: upload.Capture, request: request})
			continue
		}
		value, err := evaluateManifestValue(upload.Foreach, map[string]any{"request": manifestRequestValues(request), "uploads": newManifestUploadValues(a.manifest.Uploads)})
		if err != nil {
			return nil, fmt.Errorf("upload %q foreach: %w", id, err)
		}
		items, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("upload %q foreach must resolve to an array", id)
		}
		for index, item := range items {
			plan = append(plan, manifestUploadIteration{id: id, index: index, foreach: true, item: item, operation: upload.Operation, capture: upload.Capture, request: request})
		}
	}
	return plan, nil
}

// BuildUploadRequests 按声明顺序展开 uploads，产出宿主需要逐个执行的步骤。协议层不发
// 任何出站请求：执行尚未发生，所以步骤表达式看到的 uploads 只有声明过的键、值为空。
// 同一次预上传里让后一步引用前一步的捕获在本层不可表达，那需要宿主分轮执行再回喂结果。
func (a manifestAdapter) BuildUploadRequests(_ context.Context, c UploadContext) ([]ManifestUploadStep, error) {
	plan, err := a.uploadPlan(c.Request)
	if err != nil {
		return nil, err
	}
	if len(plan) == 0 {
		return nil, nil
	}
	uploads := newManifestUploadValues(a.manifest.Uploads)
	steps := make([]ManifestUploadStep, 0, len(plan))
	for _, iteration := range plan {
		spec, err := buildManifestOperation(iteration.operation, a.manifest.Auth, manifestOperationInput{
			Request: iteration.request,
			Uploads: uploads,
			Extras:  map[string]any{"item": iteration.item, "index": iteration.index},
		})
		if err != nil {
			return nil, fmt.Errorf("upload %q step %d: %w", iteration.id, iteration.index, err)
		}
		if err := spec.Validate(); err != nil {
			return nil, fmt.Errorf("upload %q step %d: %w", iteration.id, iteration.index, err)
		}
		steps = append(steps, ManifestUploadStep{ID: iteration.id, Index: iteration.index, Request: spec})
	}
	return steps, nil
}

// ApplyUploadResults 把宿主执行并解码后的响应交给 manifest 的 capture 映射，按声明顺序
// 重放同一份迭代计划，因此每个 capture 都能看到此前已完成的捕获（含同一 id 的已完成前缀）。
// 缺结果、未知 id 或重复下标都是宿主与计划不一致的真实故障，必须报错，不能静默留下空洞。
func (a manifestAdapter) ApplyUploadResults(_ context.Context, c RequestContext, results []ManifestUploadResult) (RequestContext, error) {
	if len(a.manifest.Uploads) == 0 {
		if len(results) > 0 {
			return RequestContext{}, fmt.Errorf("protocol %s declares no uploads but received %d upload results", a.manifest.Metadata.ID, len(results))
		}
		return c, nil
	}
	plan, err := a.uploadPlan(c.Request)
	if err != nil {
		return RequestContext{}, err
	}
	pending, err := indexManifestUploadResults(a.manifest.Uploads, plan, results)
	if err != nil {
		return RequestContext{}, err
	}
	captured := newManifestUploadValues(a.manifest.Uploads)
	requestValues := manifestRequestValues(c.Request)
	for _, iteration := range plan {
		result, ok := pending[manifestUploadKey(iteration.id, iteration.index)]
		if !ok {
			return RequestContext{}, fmt.Errorf("upload %q step %d has no result", iteration.id, iteration.index)
		}
		env := map[string]any{
			"response": result.Values,
			"item":     iteration.item,
			"index":    iteration.index,
			"request":  requestValues,
			"uploads":  cloneManifestUploadValues(captured),
		}
		entry, err := applyManifestUploadCapture(iteration.capture, env)
		if err != nil {
			return RequestContext{}, fmt.Errorf("upload %q step %d: %w", iteration.id, iteration.index, err)
		}
		if iteration.foreach {
			items, _ := captured[iteration.id].([]any)
			captured[iteration.id] = append(items, entry)
			continue
		}
		captured[iteration.id] = entry
	}
	c.Uploads = captured
	return c, nil
}

// applyManifestUploadCapture 逐个求值 capture 的具名字段：键名是字面量，不参与表达式
// 语义；求值为 nil 的字段省略，与 manifest 其余映射的丢空语义一致。
func applyManifestUploadCapture(capture map[string]any, env map[string]any) (map[string]any, error) {
	result := make(map[string]any, len(capture))
	for key, template := range capture {
		name := strings.TrimSpace(key)
		if name == "" {
			return nil, fmt.Errorf("capture field name must not be empty")
		}
		value, err := evaluateManifestValue(template, env)
		if err != nil {
			return nil, fmt.Errorf("capture field %q: %w", name, err)
		}
		if value != nil {
			result[name] = value
		}
	}
	return result, nil
}

// newManifestUploadValues 给出声明的骨架：带 foreach 的 id 是数组，不带的就是单个对象。
// BuildUploadRequests 用它作执行前的空快照，ApplyUploadResults 在其上逐项填充。
func newManifestUploadValues(uploads []ManifestUpload) map[string]any {
	result := make(map[string]any, len(uploads))
	for _, upload := range uploads {
		id := strings.TrimSpace(upload.ID)
		if upload.Foreach != nil {
			result[id] = []any{}
			continue
		}
		result[id] = map[string]any{}
	}
	return result
}

// cloneManifestUploadValues snapshots the progress so an earlier capture cannot
// be rewritten through the environment a later capture is evaluated with.
func cloneManifestUploadValues(values map[string]any) map[string]any {
	result := make(map[string]any, len(values))
	for key, value := range values {
		if items, ok := value.([]any); ok {
			copied := make([]any, len(items))
			copy(copied, items)
			result[key] = copied
			continue
		}
		result[key] = value
	}
	return result
}

func manifestUploadEnvValues(values map[string]any) map[string]any {
	if values == nil {
		return map[string]any{}
	}
	return values
}

func manifestUploadKey(id string, index int) string {
	return id + "\x00" + strconv.Itoa(index)
}

func indexManifestUploadResults(uploads []ManifestUpload, plan []manifestUploadIteration, results []ManifestUploadResult) (map[string]ManifestUploadResult, error) {
	declared := make(map[string]struct{}, len(uploads))
	for _, upload := range uploads {
		declared[strings.TrimSpace(upload.ID)] = struct{}{}
	}
	expected := make(map[string]struct{}, len(plan))
	for _, iteration := range plan {
		expected[manifestUploadKey(iteration.id, iteration.index)] = struct{}{}
	}
	indexed := make(map[string]ManifestUploadResult, len(results))
	for _, result := range results {
		id := strings.TrimSpace(result.ID)
		if _, ok := declared[id]; !ok {
			return nil, fmt.Errorf("upload result %q is not declared by the manifest", id)
		}
		key := manifestUploadKey(id, result.Index)
		if _, ok := expected[key]; !ok {
			return nil, fmt.Errorf("upload result %q has no step at index %d", id, result.Index)
		}
		if _, duplicate := indexed[key]; duplicate {
			return nil, fmt.Errorf("duplicate upload result for %q at index %d", id, result.Index)
		}
		indexed[key] = result
	}
	return indexed, nil
}

func (a manifestAdapter) parse(payload map[string]any, c PollContext) CreateResult {
	response := a.manifest.Response
	// baseUrl 让响应映射能把上游给的相对路径拼成可访问的绝对地址：本地部署的
	// ComfyUI 只回 subfolder/filename，结果端点本身在同一个 origin 下。
	env := map[string]any{"response": payload, "taskId": c.TaskID, "baseUrl": c.BaseURL, "request": manifestRequestValues(c.Request), "uploads": manifestUploadEnvValues(c.Uploads)}
	id := manifestResponseString(response.TaskID, env)
	if id == "" {
		id = firstPathValue(payload, response.TaskIDPaths...)
	}
	if id == "" {
		id = c.TaskID
	}
	statusText := manifestResponseString(response.Status, env)
	if statusText == "" {
		statusText = firstPathValue(payload, response.StatusPaths...)
	}
	status := normalizeStatus(statusText)
	if status == "" {
		status = StatusPending
	}
	message := manifestResponseString(response.Message, env)
	if message == "" {
		message = firstPathValue(payload, response.MessagePaths...)
	}
	if manifestError(payload, response.ErrorPaths...) {
		status = StatusFailed
	}
	result := &Result{
		Text:      manifestResponseString(response.Text, env),
		Reasoning: manifestResponseString(response.Reasoning, env),
	}
	if result.Text == "" {
		result.Text = firstPathValue(payload, response.TextPaths...)
	}
	if result.Reasoning == "" {
		result.Reasoning = firstPathValue(payload, response.ReasoningPaths...)
	}
	result.Images = manifestResponseMedia(response.Images, env, "image", response.ResultEphemeral)
	result.Videos = manifestResponseMedia(response.Videos, env, "video", response.ResultEphemeral)
	result.Audios = manifestResponseMedia(response.Audios, env, "audio", response.ResultEphemeral)
	// WhatsToken returns completed /v1/videos results as data[].url. Older
	// channel-1 manifests only declared the common top-level URL fields, so
	// accept this stable gateway shape without requiring a package migration.
	if a.manifest.Metadata.ID == "newapi-channel-1" && len(result.Videos) == 0 {
		for _, item := range arrayValue(payload["data"]) {
			if object, ok := item.(map[string]any); ok {
				if value := firstString(object, "url", "video_url", "videoUrl"); value != "" {
					result.Videos = append(result.Videos, MediaReference{URL: value, Kind: "video", Ephemeral: response.ResultEphemeral})
				}
			}
		}
	}
	if response.Usage != nil {
		if value, err := evaluateManifestValue(response.Usage, env); err == nil {
			result.Usage = manifestObject(value)
		}
	}
	paths := append([]string{}, a.manifest.Response.ResultURLPaths...)
	paths = append(paths, a.manifest.Response.ResultPaths...)
	for _, path := range paths {
		for _, value := range mediaPathValues(payload, path) {
			item := MediaReference{URL: value, Kind: a.manifest.Response.ResultKind, Ephemeral: a.manifest.Response.ResultEphemeral}
			switch a.manifest.Response.ResultKind {
			case "image":
				result.Images = append(result.Images, item)
			case "audio":
				result.Audios = append(result.Audios, item)
			default:
				result.Videos = append(result.Videos, item)
			}
		}
	}
	if status == StatusPending && (result.Text != "" || len(result.Images) > 0 || len(result.Videos) > 0 || len(result.Audios) > 0) {
		status = StatusSucceeded
	}
	if result.Text == "" && result.Reasoning == "" && len(result.Images) == 0 && len(result.Videos) == 0 && len(result.Audios) == 0 {
		result = nil
	}
	return CreateResult{TaskID: id, Status: status, Result: result, Message: message}
}

func validateManifestRequest(rules []ManifestValidation, request GenerationRequest) error {
	if len(rules) == 0 {
		return nil
	}
	env := map[string]any{"request": manifestRequestValues(request)}
	for _, rule := range rules {
		value, err := evaluateManifestValue(rule.Assert, env)
		if err != nil {
			return fmt.Errorf("协议参数校验表达式错误：%w", err)
		}
		if !manifestTruthy(value) {
			return fmt.Errorf("%s", strings.TrimSpace(rule.Message))
		}
	}
	return nil
}

func manifestResponseString(template any, env map[string]any) string {
	if template == nil {
		return ""
	}
	value, err := evaluateManifestValue(template, env)
	if err != nil {
		return ""
	}
	items := manifestArray(value)
	parts := make([]string, 0, len(items))
	for _, item := range items {
		if text := strings.TrimSpace(manifestString(item)); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "")
}

func manifestResponseMedia(template any, env map[string]any, kind string, ephemeral bool) []MediaReference {
	if template == nil {
		return nil
	}
	value, err := evaluateManifestValue(template, env)
	if err != nil {
		return nil
	}
	return mediaReferencesFromManifestValue(value, kind, ephemeral)
}

func mediaReferencesFromManifestValue(value any, kind string, ephemeral bool) []MediaReference {
	result := make([]MediaReference, 0)
	for _, item := range manifestArray(value) {
		switch typed := item.(type) {
		case string:
			trimmed := strings.TrimSpace(typed)
			if trimmed == "" {
				continue
			}
			reference := MediaReference{Kind: kind, Ephemeral: ephemeral}
			if strings.HasPrefix(trimmed, "data:") {
				reference.DataURL = trimmed
			} else {
				reference.URL = trimmed
			}
			result = append(result, reference)
		case map[string]any:
			reference := MediaReference{
				ID: manifestString(typed["id"]), URL: manifestString(typed["url"]), DataURL: manifestString(typed["dataUrl"]),
				Kind: defaultValue(manifestString(typed["kind"]), kind), Role: manifestString(typed["role"]), MIMEType: manifestString(typed["mimeType"]),
				Name: manifestString(typed["name"]), Order: manifestInt(typed["order"]), Weight: manifestFloat(typed["weight"]), Ephemeral: ephemeral || manifestTruthy(typed["ephemeral"]),
			}
			if reference.URL == "" {
				reference.URL = firstString(typed, "file_url", "fileUrl", "image_url", "imageUrl", "video_url", "videoUrl", "audio_url", "audioUrl", "uri")
			}
			if reference.DataURL == "" {
				reference.DataURL = firstString(typed, "data_url", "b64_json")
			}
			// OpenAI / Ark 等渠道常直接返回裸 b64_json；声明式结果下载要求 data URL。
			reference.DataURL = normalizeManifestInlineDataURL(reference.DataURL, reference.Kind, firstString(typed, "output_format", "mime_type", "mimeType"))
			if reference.URL != "" || reference.DataURL != "" {
				result = append(result, reference)
			}
		}
	}
	return result
}

func normalizeManifestInlineDataURL(value, kind, formatHint string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "data:") {
		return value
	}
	return "data:" + manifestInlineMediaMIME(kind, formatHint) + ";base64," + value
}

func manifestInlineMediaMIME(kind, formatHint string) string {
	hint := strings.ToLower(strings.TrimSpace(formatHint))
	switch hint {
	case "image/png", "image/jpeg", "image/webp", "image/gif", "audio/mpeg", "audio/wav", "audio/ogg", "video/mp4", "video/webm":
		return hint
	case "image/jpg":
		return "image/jpeg"
	case "audio/mp3":
		return "audio/mpeg"
	case "png":
		return "image/png"
	case "jpeg", "jpg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	case "gif":
		return "image/gif"
	case "mp3", "mpeg":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "mp4":
		return "video/mp4"
	case "webm":
		return "video/webm"
	}
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "audio":
		return "audio/mpeg"
	case "video":
		return "video/mp4"
	default:
		return "image/png"
	}
}

var (
	manifestMDImageRegex   = regexp.MustCompile(`!\[[^\]]*\]\(([^)\s]+)\)`)
	manifestHTMLVideoRegex = regexp.MustCompile(`(?i)<video[^>]+src=['"]([^'"]+)['"]`)
)

func mediaPathValues(payload map[string]any, path string) []string {
	value := pathValue(payload, path)
	if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
		trimmed := strings.TrimSpace(text)
		if matches := manifestMDImageRegex.FindAllStringSubmatch(trimmed, -1); len(matches) > 0 {
			var res []string
			for _, m := range matches {
				if len(m) > 1 && m[1] != "" {
					res = append(res, m[1])
				}
			}
			if len(res) > 0 {
				return res
			}
		}
		if matches := manifestHTMLVideoRegex.FindAllStringSubmatch(trimmed, -1); len(matches) > 0 {
			var res []string
			for _, m := range matches {
				if len(m) > 1 && m[1] != "" {
					res = append(res, m[1])
				}
			}
			if len(res) > 0 {
				return res
			}
		}
		return []string{trimmed}
	}
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		switch typed := item.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				values = append(values, strings.TrimSpace(typed))
			}
		case map[string]any:
			if url := firstString(typed, "url", "file_url", "fileUrl", "video_url", "videoUrl", "image_url", "imageUrl"); url != "" {
				values = append(values, url)
			}
		}
	}
	return values
}

// manifestOperationInput is the expression input for one outbound request. The
// "uploads" environment key is always present, so a manifest that reads a
// capture sees a stable shape instead of a missing key. Extras carry the
// per-iteration bindings the uploads pass needs ("item", "index") without
// widening every other call site.
type manifestOperationInput struct {
	Request GenerationRequest
	TaskID  string
	Uploads map[string]any
	Extras  map[string]any
}

func buildManifestOperation(operation ManifestOperation, auth ManifestAuth, input manifestOperationInput) (RequestSpec, error) {
	request, taskID := input.Request, input.TaskID
	requestValues := manifestRequestValues(request)
	env := map[string]any{"request": requestValues, "taskId": taskID, "uploads": manifestUploadEnvValues(input.Uploads)}
	for key, value := range input.Extras {
		env[key] = value
	}
	var body any
	if operation.Body != nil {
		value, err := evaluateManifestValue(operation.Body, env)
		if err != nil {
			return RequestSpec{}, fmt.Errorf("evaluate request body: %w", err)
		}
		body = normalizeManifestValue(value)
	} else if len(operation.Fields) > 0 {
		legacyBody := make(map[string]any, len(operation.Fields))
		for key, expression := range operation.Fields {
			value := manifestExpressionValue(expression, request, taskID)
			if value == nil {
				continue
			}
			setMapPath(legacyBody, key, value)
		}
		body = normalizeManifestValue(legacyBody)
	}
	pathValue := any(operation.Path)
	if operation.PathTemplate != nil {
		pathValue = operation.PathTemplate
	}
	evaluatedPath, err := evaluateManifestValue(pathValue, env)
	if err != nil {
		return RequestSpec{}, fmt.Errorf("evaluate request path: %w", err)
	}
	path := strings.ReplaceAll(manifestString(evaluatedPath), "{{taskId}}", url.PathEscape(taskID))
	path = strings.ReplaceAll(path, "{{model}}", url.PathEscape(request.Model))
	path = interpolateManifestString(path, env)
	if !isRelativePath(path) {
		return RequestSpec{}, fmt.Errorf("evaluated request path must be relative: %q", path)
	}
	headers, err := evaluateManifestStringMap(operation.Headers, env)
	if err != nil {
		return RequestSpec{}, fmt.Errorf("evaluate request headers: %w", err)
	}
	query, err := evaluateManifestQuery(operation.Query, env)
	if err != nil {
		return RequestSpec{}, fmt.Errorf("evaluate request query: %w", err)
	}
	files, err := evaluateManifestFiles(operation.Files, env)
	if err != nil {
		return RequestSpec{}, err
	}
	contentTypeValue := any(operation.ContentType)
	if operation.ContentTypeTemplate != nil {
		contentTypeValue = operation.ContentTypeTemplate
	}
	evaluatedContentType, err := evaluateManifestValue(contentTypeValue, env)
	if err != nil {
		return RequestSpec{}, fmt.Errorf("evaluate request content type: %w", err)
	}
	contentType := defaultValue(manifestString(evaluatedContentType), "application/json")
	return RequestSpec{Method: strings.ToUpper(operation.Method), Path: path, OriginPath: operation.OriginPath, ContentType: contentType, Headers: headers, Query: query, Body: body, Files: files, Auth: auth}, nil
}

func evaluateManifestStringMap(values map[string]any, env map[string]any) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	result := make(map[string]string, len(values))
	for key, template := range values {
		value, err := evaluateManifestValue(template, env)
		if err != nil {
			return nil, err
		}
		if text := strings.TrimSpace(manifestString(value)); text != "" {
			result[key] = text
		}
	}
	return result, nil
}

func evaluateManifestQuery(values map[string]any, env map[string]any) (map[string][]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	result := make(map[string][]string, len(values))
	for key, template := range values {
		value, err := evaluateManifestValue(template, env)
		if err != nil {
			return nil, err
		}
		for _, item := range manifestArray(value) {
			if text := strings.TrimSpace(manifestString(item)); text != "" {
				result[key] = append(result[key], text)
			}
		}
	}
	return result, nil
}

func evaluateManifestFiles(parts []ManifestFilePart, env map[string]any) ([]RequestFilePart, error) {
	result := make([]RequestFilePart, 0)
	for _, part := range parts {
		source, err := evaluateManifestValue(part.Source, env)
		if err != nil {
			return nil, fmt.Errorf("evaluate multipart file %q: %w", part.Name, err)
		}
		filename, err := evaluateManifestValue(part.Filename, env)
		if err != nil {
			return nil, fmt.Errorf("evaluate multipart filename %q: %w", part.Name, err)
		}
		mimeType, err := evaluateManifestValue(part.MIMEType, env)
		if err != nil {
			return nil, fmt.Errorf("evaluate multipart MIME type %q: %w", part.Name, err)
		}
		for index, value := range manifestArray(source) {
			references := mediaReferencesFromManifestValue(value, "file", false)
			for _, reference := range references {
				name := strings.TrimSpace(manifestString(filename))
				if name == "" {
					name = reference.Name
				}
				if name == "" {
					name = fmt.Sprintf("%s-%d", part.Name, index+1)
				}
				contentType := strings.TrimSpace(manifestString(mimeType))
				if contentType == "" {
					contentType = reference.MIMEType
				}
				result = append(result, RequestFilePart{Name: part.Name, Filename: name, MIMEType: contentType, Reference: reference})
			}
		}
	}
	return result, nil
}

func normalizeManifestValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		if len(v) == 0 {
			return v
		}
		isSequential := true
		for i := 0; i < len(v); i++ {
			if _, ok := v[strconv.Itoa(i)]; !ok {
				isSequential = false
				break
			}
		}
		if isSequential {
			arr := make([]any, len(v))
			for i := 0; i < len(v); i++ {
				arr[i] = normalizeManifestValue(v[strconv.Itoa(i)])
			}
			return arr
		}
		res := make(map[string]any, len(v))
		for k, val := range v {
			res[k] = normalizeManifestValue(val)
		}
		return res
	case []any:
		arr := make([]any, len(v))
		for i, val := range v {
			arr[i] = normalizeManifestValue(val)
		}
		return arr
	default:
		return value
	}
}

func manifestExpressionValue(expression string, request GenerationRequest, taskID string) any {
	expression = strings.TrimSpace(expression)
	source := expression
	transforms := []string{}
	if separator := strings.IndexByte(expression, '|'); separator >= 0 {
		source = strings.TrimSpace(expression[:separator])
		transforms = strings.Split(expression[separator+1:], "|")
	}

	var value any
	switch {
	case source == "taskId":
		value = taskID
	case strings.HasPrefix(source, "request."):
		value = pathValue(manifestRequestValues(request), strings.TrimPrefix(source, "request."))
	default:
		value = source
	}
	for _, transform := range transforms {
		value = applyManifestTransform(value, transform, request)
		if value == nil {
			break
		}
	}
	return value
}

// manifestRequestValues is deliberately a small, JSON-shaped view of the
// platform request. It lets uploaded manifests address media items by index
// without exposing Go structs or adding provider-specific host code.
func manifestRequestValues(request GenerationRequest) map[string]any {
	userContent := make([]any, 0)
	if strings.TrimSpace(request.Prompt) != "" {
		userContent = append(userContent, map[string]any{"type": "text", "text": request.Prompt})
	}
	for _, img := range request.Images {
		val := defaultValue(img.URL, img.DataURL)
		if val != "" {
			userContent = append(userContent, map[string]any{"type": "image_url", "image_url": map[string]any{"url": val}})
		}
	}
	for _, vid := range request.Videos {
		val := defaultValue(vid.URL, vid.DataURL)
		if val != "" {
			userContent = append(userContent, map[string]any{"type": "video_url", "video_url": map[string]any{"url": val}})
		}
	}
	for _, aud := range request.Audios {
		val := defaultValue(aud.URL, aud.DataURL)
		if val != "" {
			userContent = append(userContent, map[string]any{"type": "audio_url", "audio_url": map[string]any{"url": val}})
		}
	}
	messages := make([]any, 0, len(request.Messages)+2)
	// instructions 是统一的系统指令字段，但多数 OpenAI 系协议只映射 request.messages。
	// 系统指令必须进入消息数组，否则会被静默丢弃；带独立 system 字段的协议（Claude、
	// Gemini、Responses）在各自模板里过滤 system 角色，不会重复发送。
	if instructions := strings.TrimSpace(request.Instructions); instructions != "" && !hasSystemMessage(request.Messages) {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	for _, message := range request.Messages {
		if strings.TrimSpace(message.Role) == "" || message.Content == nil {
			continue
		}
		messages = append(messages, map[string]any{"role": message.Role, "content": message.Content})
	}
	if len(userContent) > 0 {
		content := any(userContent)
		if len(userContent) == 1 && len(request.Images)+len(request.Videos)+len(request.Audios) == 0 {
			content = request.Prompt
		}
		messages = append(messages, map[string]any{"role": "user", "content": content})
	}

	inputs := request.Inputs
	if len(inputs) == 0 {
		inputs = append(inputs, request.Images...)
		inputs = append(inputs, request.Videos...)
		inputs = append(inputs, request.Audios...)
	}
	output := request.Output
	if output.Count == 0 {
		output.Count = request.ImageCount
	}
	if output.Duration == 0 {
		output.Duration = request.Duration
	}
	if output.AspectRatio == "" {
		output.AspectRatio = request.AspectRatio
	}
	if output.Resolution == "" {
		output.Resolution = request.Resolution
	}
	if output.Quality == "" {
		output.Quality = request.Quality
	}
	output.GenerateAudio = output.GenerateAudio || request.GenerateAudio
	output.Watermark = output.Watermark || request.Watermark
	outputValue, _ := requestAsManifestValue(output)

	return map[string]any{
		"capability":      request.Capability,
		"model":           request.Model,
		"prompt":          request.Prompt,
		"instructions":    request.Instructions,
		"messages":        messages,
		"inputs":          manifestMediaValues(inputs),
		"images":          manifestMediaValues(request.Images),
		"videos":          manifestMediaValues(request.Videos),
		"audios":          manifestMediaValues(request.Audios),
		"imageCount":      request.ImageCount,
		"duration":        request.Duration,
		"aspectRatio":     request.AspectRatio,
		"resolution":      request.Resolution,
		"quality":         request.Quality,
		"generateAudio":   request.GenerateAudio,
		"watermark":       request.Watermark,
		"operation":       request.Operation,
		"output":          outputValue,
		"providerOptions": request.ProviderOptions,
		"extra":           request.Extra,
		"workflow":        manifestWorkflowValues(request.Workflow),
	}
}

// manifestWorkflowValues projects the host-mapped workflow graph into the JSON
// view of the request. An absent workflow stays nil instead of an empty object,
// so "$ref: request.workflow.json" resolves to nothing rather than to a node
// every manifest would have to test for.
func manifestWorkflowValues(workflow *WorkflowPayload) any {
	if workflow == nil {
		return nil
	}
	value, err := requestAsManifestValue(workflow)
	if err != nil {
		return nil
	}
	return value
}

func hasSystemMessage(messages []Message) bool {
	for _, message := range messages {
		if strings.EqualFold(strings.TrimSpace(message.Role), "system") {
			return true
		}
	}
	return false
}

func requestAsManifestValue(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var result any
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func manifestModelID(request GenerationRequest) string {
	modelID := strings.TrimSpace(request.Model)
	if separator := strings.LastIndex(modelID, "::"); separator >= 0 {
		modelID = modelID[separator+2:]
	}
	return strings.ToLower(modelID)
}

func manifestMediaValues(values []MediaReference) []any {
	result := make([]any, 0, len(values))
	for index, value := range values {
		order := value.Order
		if order == 0 && index > 0 {
			order = index
		}
		resolved := defaultValue(value.URL, value.DataURL)
		result = append(result, map[string]any{
			"id":       value.ID,
			"url":      value.URL,
			"dataUrl":  value.DataURL,
			"value":    resolved,
			"kind":     value.Kind,
			"role":     value.Role,
			"mimeType": value.MIMEType,
			"name":     value.Name,
			"order":    order,
			"weight":   value.Weight,
			"metadata": value.Metadata,
			"source": map[string]any{
				"type":     manifestMediaSourceType(value),
				"value":    resolved,
				"mimeType": value.MIMEType,
			},
			"ephemeral": value.Ephemeral,
		})
	}
	return result
}

func manifestMediaSourceType(value MediaReference) string {
	if value.DataURL != "" {
		return "data"
	}
	if value.URL != "" {
		return "url"
	}
	return "unknown"
}

func applyManifestTransform(value any, transform string, request GenerationRequest) any {
	transform = strings.ToLower(strings.TrimSpace(transform))
	if transform == "bool" || transform == "boolean" {
		switch v := value.(type) {
		case bool:
			return v
		case string:
			s := strings.ToLower(strings.TrimSpace(v))
			return s == "true" || s == "1" || s == "yes"
		case int, int64, float64:
			return v != 0
		default:
			return false
		}
	}
	if transform == "int" || transform == "integer" {
		switch v := value.(type) {
		case int:
			return v
		case int64:
			return int(v)
		case float64:
			return int(v)
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return n
			}
			return 0
		default:
			return 0
		}
	}
	if transform == "omit_zero" {
		switch v := value.(type) {
		case int:
			if v == 0 {
				return nil
			}
		case int64:
			if v == 0 {
				return nil
			}
		case float64:
			if v == 0 {
				return nil
			}
		case string:
			if strings.TrimSpace(v) == "0" || strings.TrimSpace(v) == "" {
				return nil
			}
		}
	}
	cleanModel := manifestModelID(request)
	if strings.HasPrefix(transform, "omit_unless_model_contains:") {
		needle := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(transform, "omit_unless_model_contains:")))
		if needle == "" || !strings.Contains(cleanModel, needle) {
			return nil
		}
	}
	if strings.HasPrefix(transform, "omit_if_model_contains:") {
		needle := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(transform, "omit_if_model_contains:")))
		if needle != "" && strings.Contains(cleanModel, needle) {
			return nil
		}
	}
	if strings.HasPrefix(transform, "omit_unless_model_equals:") {
		target := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(transform, "omit_unless_model_equals:")))
		if target == "" || cleanModel != target {
			return nil
		}
	}
	if strings.HasPrefix(transform, "omit_if_model_equals:") {
		target := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(transform, "omit_if_model_equals:")))
		if target != "" && cleanModel == target {
			return nil
		}
	}
	text, ok := value.(string)
	if transform == "omit_empty" && (!ok || strings.TrimSpace(text) == "") {
		return nil
	}
	if transform == "omit_auto" && (!ok || strings.TrimSpace(text) == "" || strings.EqualFold(strings.TrimSpace(text), "auto") || strings.EqualFold(strings.TrimSpace(text), "default")) {
		return nil
	}
	if !ok {
		return value
	}
	switch transform {
	case "trim":
		return strings.TrimSpace(text)
	case "lower", "lowercase":
		return strings.ToLower(text)
	case "upper", "uppercase":
		return strings.ToUpper(text)
	case "resolution_p":
		if regexp.MustCompile(`^\d+$`).MatchString(strings.TrimSpace(text)) {
			return strings.TrimSpace(text) + "p"
		}
		return text
	case "video-resolution", "video_resolution", "videoresolution":
		// Compatibility for already-installed 1.0.1 manifests. New plugins
		// should declare exact enum values and use generic string transforms.
		return normalizeLegacyManifestVideoResolution(text)
	default:
		return value
	}
}

func normalizeLegacyManifestVideoResolution(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return ""
	}
	for _, char := range normalized {
		if char < '0' || char > '9' {
			return normalized
		}
	}
	return normalized + "p"
}

func pathValue(payload map[string]any, path string) any {
	return manifestPathValue(payload, path)
}

func arrayValue(value any) []any {
	items, _ := value.([]any)
	return items
}

func firstPathValue(payload map[string]any, paths ...string) string {
	for _, path := range paths {
		value := pathValue(payload, path)
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func manifestError(payload map[string]any, paths ...string) bool {
	for _, path := range paths {
		value := pathValue(payload, path)
		switch typed := value.(type) {
		case nil:
			continue
		case string:
			switch strings.ToLower(strings.TrimSpace(typed)) {
			case "", "0", "ok", "success", "succeeded", "true":
				continue
			default:
				return true
			}
		case float64:
			if typed == 0 {
				continue
			}
			return true
		case float32:
			if typed == 0 {
				continue
			}
			return true
		case int, int8, int16, int32, int64:
			if reflectValueIsZero(typed) {
				continue
			}
			return true
		case uint, uint8, uint16, uint32, uint64:
			if reflectValueIsZero(typed) {
				continue
			}
			return true
		default:
			return true
		}
	}
	return false
}

func reflectValueIsZero(value any) bool {
	switch typed := value.(type) {
	case int:
		return typed == 0
	case int8:
		return typed == 0
	case int16:
		return typed == 0
	case int32:
		return typed == 0
	case int64:
		return typed == 0
	case uint:
		return typed == 0
	case uint8:
		return typed == 0
	case uint16:
		return typed == 0
	case uint32:
		return typed == 0
	case uint64:
		return typed == 0
	default:
		return false
	}
}

// firstPathJSONValue keeps scalar response fields string-like while allowing
// providers to return tool arguments as an object instead of a JSON string.
// The platform tool contract always stores arguments as JSON text.
func firstPathJSONValue(payload map[string]any, paths ...string) string {
	for _, path := range paths {
		value := pathValue(payload, path)
		switch typed := value.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				return strings.TrimSpace(typed)
			}
		case nil:
			continue
		default:
			encoded, err := json.Marshal(typed)
			if err == nil && len(encoded) > 0 {
				return string(encoded)
			}
		}
	}
	return ""
}

func setMapPath(target map[string]any, path string, value any) {
	parts := strings.Split(strings.Trim(path, "."), ".")
	current := target
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[part] = next
		}
		current = next
	}
	if len(parts) > 0 {
		current[parts[len(parts)-1]] = value
	}
}

func isRelativePath(path string) bool {
	parsed, err := url.Parse(strings.TrimSpace(path))
	return err == nil && parsed.Path != "" && strings.HasPrefix(parsed.Path, "/") && !strings.HasPrefix(parsed.Path, "//") && parsed.Host == "" && parsed.User == nil && parsed.Scheme == ""
}

func validManifestIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 96 {
		return false
	}
	for index, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
			if index == 0 && (char == '-' || char == '_' || char == '.') {
				return false
			}
			continue
		}
		return false
	}
	return true
}
