package plugins

import (
	"strings"

	"infinite-canvas/backend/internal/protocol"
)

func BundledWorkflowManifests() []protocol.Manifest {
	return []protocol.Manifest{
		workflowPluginManifest(WorkflowRunningHub, "RunningHub 工作流", "在画布中拉取并执行 RunningHub Workflow 与 App。",
			[]protocol.Capability{protocol.CapabilityImage, protocol.CapabilityVideo, protocol.CapabilityAudio}),
		workflowPluginManifest(WorkflowComfyUI, "ComfyUI 工作流", "连接本地或可信网络内的原生 ComfyUI，按工作流字段生成图片和视频。",
			[]protocol.Capability{protocol.CapabilityImage, protocol.CapabilityVideo}),
	}
}

// bundledWorkflowPluginIDs 是所有「执行落在宿主 Go 代码、生命周期交给插件运行时」的
// 工作流能力。顺序同时决定插件列表与状态返回顺序。
func BundledWorkflowPluginIDs() []string {
	return []string{WorkflowRunningHub, WorkflowComfyUI}
}

// WorkflowDisplayName 用于插件门禁失败时的可读提示。
func WorkflowDisplayName(pluginID string) string {
	switch pluginID {
	case WorkflowRunningHub:
		return "RunningHub 工作流插件"
	case WorkflowComfyUI:
		return "ComfyUI 工作流插件"
	default:
		return "工作流插件"
	}
}

func workflowPluginManifest(id, name, description string, capabilities []protocol.Capability) protocol.Manifest {
	workflows := make([]protocol.ManifestWorkflow, 0, len(capabilities))
	for _, capability := range capabilities {
		workflows = append(workflows, protocol.ManifestWorkflow{
			ID:         id + "-" + string(capability),
			Label:      name + " · " + string(capability),
			ProviderID: id,
			Capability: capability,
			Parameters: []protocol.Parameter{},
		})
	}
	return protocol.Manifest{
		APIVersion: "beeftv.plugin/v1",
		Metadata: protocol.Metadata{
			ID:            id,
			Version:       "1.0.0",
			Name:          name,
			Vendor:        "内置工作流",
			Description:   description,
			Documentation: "# " + name + "\n\n## 宿主运行时合同\n\n该工作流能力由插件运行时统一管理。",
			Enabled:       false,
			Installable:   true,
		},
		Surfaces:    []string{"node", "settings"},
		Runtime:     protocol.ManifestRuntime{Backend: "trusted-backend", Web: "trusted-backend"},
		Permissions: []string{"generation.run", "external.open"},
		Contributes: protocol.ManifestContributions{Workflows: workflows},
	}
}

func WorkflowIDForInterface(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "runninghub-workflow-image", "runninghub-workflow-video", "runninghub-workflow-audio":
		return WorkflowRunningHub, true
	case "comfyui-workflow-image", "comfyui-workflow-video":
		return WorkflowComfyUI, true
	default:
		return "", false
	}
}
