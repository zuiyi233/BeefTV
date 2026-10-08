// Package assistant holds the canonical assistant-provider value, reason, and
// fingerprint contracts. Selection stays in app; process lifecycle stays in
// assistantruntime. Neither domain imports the other.
package assistant

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// 助手不可用的机器可读原因；UI 按 reason 映射文案，不解析 msg。
const (
	ReasonModelNotConfigured  = "model_not_configured"
	ReasonCredentialMissing   = "credential_missing"
	ReasonProtocolUnsupported = "model_protocol_unsupported"
)

// ReasonSessionModelChanged 由宿主在「这条会话是在别的模型下开始的，
// 在途 checkpoint 仍引用已经不存在的模型」时回报（契约字符串，不得改名）。
// 它和上面三条不同层：模型本身是好的，坏的只是这条旧会话，
// 所以 /assistant/status 必须继续声明 available:true，只额外带上会话级提示。
const ReasonSessionModelChanged = "session_model_changed"

// SessionModelChangedMessage 是后端信封里给这条原因的固定中文说明。
// UI 仍按 reason 判定行为；msg 只保证即使界面没接新字段也不会显示成通用失败。
const SessionModelChangedMessage = "这一轮开始时用的是另一个模型，创作助手没有执行它。现在已改用当前模型，重新发送这条要求即可继续；也可以新建对话，历史仍然可以查看。"

// HostTurnReasonMessage 把宿主回报的回合原因映射成可公开的中文说明。
// 未知原因返回 false：宿主正文不进响应，调用方保留自己的通用文案。
func HostTurnReasonMessage(reason string) (string, bool) {
	if strings.TrimSpace(reason) != ReasonSessionModelChanged {
		return "", false
	}
	return SessionModelChangedMessage, true
}

// UnavailableError 承载稳定 reason，让 handler 直接投影成契约里的状态。
type UnavailableError struct {
	Reason  string
	Message string
}

func (e *UnavailableError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Message) != "" {
		return e.Message
	}
	return e.Reason
}

// Provider 是助手宿主要用的一次性连接信息（含密钥，只在进程内传递）。
type Provider struct {
	ChannelID   string
	ChannelName string
	// Model 是渠道内的模型 id（不含 channel:: 前缀）；ModelKey 是配置里的原值。
	Model    string
	ModelKey string
	Protocol string
	BaseURL  string
	APIKey   string
	// Headers 是上游要求的额外请求头（例如订阅渠道的账号隔离头）。
	// 与 APIKey 一样只在进程内传递，不写日志、不落盘。
	Headers map[string]string
}

// Fingerprint 是「当前生效的供应商配置」的稳定指纹：变化即说明宿主环境已过期。
// 密钥只进哈希，不进任何可读输出。
func (p Provider) Fingerprint() string {
	sum := sha256.Sum256([]byte(p.APIKey))
	// 额外请求头（例如账号 id）变化时宿主必须重启，因此一并纳入指纹。
	headerKeys := make([]string, 0, len(p.Headers))
	for key := range p.Headers {
		headerKeys = append(headerKeys, key)
	}
	sort.Strings(headerKeys)
	headerParts := make([]string, 0, len(headerKeys))
	for _, key := range headerKeys {
		headerSum := sha256.Sum256([]byte(p.Headers[key]))
		headerParts = append(headerParts, key+"="+hex.EncodeToString(headerSum[:]))
	}
	digest := sha256.Sum256([]byte(strings.Join([]string{
		p.ChannelID, p.Model, p.BaseURL, p.Protocol, hex.EncodeToString(sum[:]),
		strings.Join(headerParts, ","),
	}, "\x00")))
	return hex.EncodeToString(digest[:])
}
