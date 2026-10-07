package protocol

import "context"

// Adapter owns only protocol translation. The host owns credentials, outbound
// security, polling leases, result downloads and task recovery.
type Adapter interface {
	Metadata() Metadata
	BuildCreate(context.Context, RequestContext) (RequestSpec, error)
	ParseCreate(context.Context, []byte) (CreateResult, error)
	BuildPoll(context.Context, PollContext) (RequestSpec, error)
	ParsePoll(context.Context, PollContext, []byte) (PollResult, error)
	BuildCancel(context.Context, PollContext) (RequestSpec, error)
}

// AgentAdapter is the optional protocol surface for tool-capable text calls.
// The host still owns credentials and outbound policy; a plugin only
// maps the platform's agent request into the provider payload and parses the
// provider response back into the platform tool-call contract.
type AgentAdapter interface {
	BuildAgent(context.Context, AgentRequestContext) (RequestSpec, error)
	ParseAgent(context.Context, []byte) (AgentResult, error)
}

// AgentCapability lets the host distinguish a declarative provider that
// actually declares an agent operation from one that only satisfies the Go
// method set through the shared manifest adapter implementation.
type AgentCapability interface {
	AgentAvailable() bool
}

// ResultAdapter optionally resolves an authenticated binary result after an
// asynchronous task succeeds without returning a public media URL.
type ResultAdapter interface {
	BuildResult(context.Context, PollContext) (RequestSpec, error)
}

type ResultCapability interface {
	ResultAvailable() bool
}

// CancelRequestsAdapter is the optional protocol surface for providers where a
// single upstream call cannot cover every task state, such as a queue that only
// removes pending work and a separate endpoint that interrupts running work.
// Requests run in the returned order; credentials, outbound policy and receipts
// stay with the host.
type CancelRequestsAdapter interface {
	BuildCancelRequests(context.Context, PollContext) ([]RequestSpec, error)
}

// UploadAdapter is the optional protocol surface for providers that have to
// receive local media before the task can be submitted, such as ComfyUI storing
// a reference image through POST /upload/image and then addressing it by the
// name, subfolder and type the server chose.
//
// The protocol only builds requests and folds decoded responses back into the
// request environment. It never performs an outbound call: credentials, SSRF
// policy, timeouts, file decoding and receipts stay with the host, exactly as
// they do for create, cancel and result. BuildUploadRequests expands every
// declared foreach source into the steps to run in order; ApplyUploadResults
// takes the decoded responses for those steps, evaluates the manifest capture
// mapping itself, and returns the context whose create/poll/cancel/result
// expressions can read the captured values through the "uploads" key.
type UploadAdapter interface {
	BuildUploadRequests(context.Context, UploadContext) ([]ManifestUploadStep, error)
	ApplyUploadResults(context.Context, RequestContext, []ManifestUploadResult) (RequestContext, error)
}

// PollPolicyAdapter exposes the poll cadence a provider declared for itself.
// The second result is false when the manifest does not declare a policy, so
// the host keeps its own rhythm instead of receiving a protocol default.
type PollPolicyAdapter interface {
	PollPolicy() (PollPolicy, bool)
}

type AgentRequestContext struct {
	BaseURL string
	Model   string
	Request map[string]any
}

type AgentToolCall struct {
	ID               string
	Name             string
	Arguments        string
	ThoughtSignature string
}

type AgentResult struct {
	Text      string
	Reasoning string
	ToolCalls []AgentToolCall
}

type UnavailableAdapter struct {
	Info Metadata
}

func (a UnavailableAdapter) Metadata() Metadata { return a.Info }

func (a UnavailableAdapter) BuildCreate(context.Context, RequestContext) (RequestSpec, error) {
	return RequestSpec{}, unavailable(a.Info)
}

func (a UnavailableAdapter) ParseCreate(context.Context, []byte) (CreateResult, error) {
	return CreateResult{}, unavailable(a.Info)
}

func (a UnavailableAdapter) BuildPoll(context.Context, PollContext) (RequestSpec, error) {
	return RequestSpec{}, unavailable(a.Info)
}

func (a UnavailableAdapter) ParsePoll(context.Context, PollContext, []byte) (PollResult, error) {
	return PollResult{}, unavailable(a.Info)
}

func (a UnavailableAdapter) BuildCancel(context.Context, PollContext) (RequestSpec, error) {
	return RequestSpec{}, unavailable(a.Info)
}
