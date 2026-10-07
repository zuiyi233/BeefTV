import { Alert, App, Button, Form, Input, Popconfirm } from "antd";
import { Switch } from "@/components/ui/base/switch";
import { Select } from "@/components/ui/base/select";
import { SegmentedControl } from "@/components/ui/base/segmented-control";
import { Plus, RefreshCw, Trash2 } from "lucide-react";
import { useEffect, useState } from "react";
import { nanoid } from "nanoid";

import { WorkflowGraphEditor } from "@/components/workflow-graph-editor";
import { comfyUIWorkflowClassTypes, comfyUIWorkflowFields, inferComfyUIPromptField, inferComfyUIReferenceImageFields, type ComfyUIWorkflowJson } from "@/lib/comfyui-workflow";
import { fetchComfyUIObjectInfo } from "@/services/api/comfyui";
import {
    normalizeComfyUICapability,
    normalizeWorkflowFieldMappings,
    useConfigStore,
    type ComfyUICapability,
    type ComfyUIConfig,
    type ComfyUIWorkflow,
    type WorkflowFieldMapping,
} from "@/stores/use-config-store";

const fallbackBaseUrl = "http://127.0.0.1:8188";

const capabilityOptions = [
    { label: "图片", value: "image" },
    { label: "视频", value: "video" },
];

function capabilityLabel(capability: ComfyUICapability) {
    return capability === "video" ? "视频" : "图片";
}

/**
 * 原生 ComfyUI 的配置入口。
 *
 * 与 RunningHub 不同，这里不需要 API Key：ComfyUI 是自托管服务，
 * 配置重点是把 API 格式工作流的节点字段映射到画布参数。
 */
export function ComfyUISettingsPane() {
    const { message } = App.useApp();
    const config = useConfigStore((state) => state.config);
    const updateConfig = useConfigStore((state) => state.updateConfig);
    const comfyui = config.comfyui;
    const selected = comfyui.workflows.find((item) => item.workflowId.trim() === comfyui.workflowId.trim());
    const [jsonText, setJsonText] = useState(selected?.workflowJson ? JSON.stringify(selected.workflowJson, null, 2) : "");
    const [title, setTitle] = useState(selected?.title || "");
    const [draftJson, setDraftJson] = useState<ComfyUIWorkflowJson | null>(null);
    const [draftFields, setDraftFields] = useState<WorkflowFieldMapping[]>([]);
    const [parsing, setParsing] = useState(false);

    useEffect(() => {
        if (!selected) return;
        setJsonText(selected.workflowJson ? JSON.stringify(selected.workflowJson, null, 2) : "");
        setTitle(selected.title || "");
    }, [selected?.workflowId, selected?.workflowJson, selected?.title]);

    const update = (patch: Partial<ComfyUIConfig>) => updateConfig("comfyui", { ...comfyui, ...patch });
    const baseUrl = comfyui.baseUrl.trim() || fallbackBaseUrl;
    const editorFields = selected ? selected.fields || [] : draftFields;
    const editorJson = selected ? selected.workflowJson : draftJson || undefined;

    const persistWorkflow = (workflow: ComfyUIWorkflow) => {
        const workflowId = workflow.workflowId.trim();
        if (!workflowId) return;
        const next: ComfyUIWorkflow = {
            ...workflow,
            workflowId,
            capability: normalizeComfyUICapability(workflow.capability, comfyui.capability),
            fields: normalizeWorkflowFieldMappings(workflow.fields, normalizeComfyUICapability(workflow.capability, comfyui.capability)),
        };
        update({
            workflowId,
            capability: next.capability,
            workflows: [...comfyui.workflows.filter((item) => item.workflowId.trim() !== workflowId), next],
        });
    };

    const parseWorkflow = async () => {
        const parsed = parseJsonObject(jsonText);
        if (!parsed) {
            message.error("工作流 JSON 解析失败，请粘贴 ComfyUI 导出的 API 格式工作流");
            return;
        }
        const classTypes = comfyUIWorkflowClassTypes(parsed);
        if (classTypes.length === 0) {
            message.error("未在工作流里识别到节点，请确认导出的是 API 格式");
            return;
        }
        setParsing(true);
        try {
            // 节点定义只用于补全枚举与数值范围；上游不可达时仍保留工作流自身的字段，
            // 用户可以手动填写，因此这里降级为提示而不是中断。
            const objectInfo = await fetchComfyUIObjectInfo(baseUrl, classTypes).catch((error: unknown) => {
                message.warning(`读取 ComfyUI 节点定义失败，已跳过枚举补全：${errorText(error)}`);
                return undefined;
            });
            // 先识别参考图槽位，再推断提示词槽位：LoadImage 与文本节点互不重叠，
            // 但顺序固定后行为更可预期。
            const fields = inferComfyUIPromptField(inferComfyUIReferenceImageFields(comfyUIWorkflowFields(parsed, objectInfo)));
            setDraftJson(parsed);
            setDraftFields(fields);
            message.success(`已解析 ${classTypes.length} 类节点，生成 ${fields.length} 个可配置字段`);
        } finally {
            setParsing(false);
        }
    };

    const saveWorkflow = () => {
        if (!draftJson) {
            message.warning("请先解析工作流 JSON");
            return;
        }
        const workflowTitle = title.trim() || `ComfyUI 工作流 ${comfyui.workflows.length + 1}`;
        const entry: ComfyUIWorkflow = {
            workflowId: nanoid(),
            title: workflowTitle,
            capability: comfyui.capability,
            workflowJson: draftJson,
            fields: draftFields,
        };
        update({ workflows: [...comfyui.workflows, entry], workflowId: entry.workflowId });
        setDraftJson(null);
        setDraftFields([]);
        message.success(`已保存「${workflowTitle}」`);
    };

    const selectWorkflow = (workflowId: string | undefined) => {
        if (!workflowId) {
            update({ workflowId: "" });
            setTitle("");
            setJsonText("");
            return;
        }
        const workflow = comfyui.workflows.find((item) => item.workflowId.trim() === workflowId);
        if (!workflow) return message.error("已保存条目不存在，请刷新页面后重试");
        const nextCapability = normalizeComfyUICapability(workflow.capability, comfyui.capability);
        update({ workflowId: workflow.workflowId.trim(), capability: nextCapability });
        setTitle(workflow.title || "");
        setJsonText(workflow.workflowJson ? JSON.stringify(workflow.workflowJson, null, 2) : "");
    };

    const removeWorkflow = (workflow: ComfyUIWorkflow) => {
        const remaining = comfyui.workflows.filter((item) => item.workflowId.trim() !== workflow.workflowId.trim());
        update({ workflowId: "", workflows: remaining });
        setDraftJson(null);
        setDraftFields([]);
        setTitle("");
        setJsonText("");
        message.success(`已删除工作流：${workflow.title || workflow.workflowId}`);
    };

    const updateWorkflowJson = (value: string) => {
        if (!selected) {
            setDraftJson(parseJsonObject(value));
            return;
        }
        try {
            const parsed = value.trim() ? JSON.parse(value) : {};
            if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("工作流 JSON 必须是对象");
            persistWorkflow({ ...selected, title: title.trim() || selected.title, workflowJson: parsed as ComfyUIWorkflowJson });
        } catch (error) {
            message.error(error instanceof Error ? error.message : "工作流 JSON 格式无效");
        }
    };

    const updateFields = (fields: WorkflowFieldMapping[]) => {
        if (selected) {
            persistWorkflow({ ...selected, title: title.trim() || selected.title, fields });
            return;
        }
        setDraftFields(fields);
    };

    const updateTitle = () => {
        if (!selected) return;
        persistWorkflow({ ...selected, title: title.trim() || selected.workflowId });
    };

    const updateCapability = (value: ComfyUICapability) => {
        const nextCapability = normalizeComfyUICapability(value, comfyui.capability);
        update({ capability: nextCapability });
        if (selected) persistWorkflow({ ...selected, capability: nextCapability });
    };

    return (
        <Form layout="vertical" requiredMark={false}>
            <div className="settings-pane-header">
                <div className="min-w-0">
                    <h2>ComfyUI 工作流</h2>
                    <p>连接本地或可信网络内的原生 ComfyUI，用 API 格式工作流生成图片与视频；工作流字段映射决定画布参数如何写入节点。</p>
                </div>
                <Switch checked={comfyui.enabled} checkedChildren="启用" unCheckedChildren="停用" onChange={(enabled) => update({ enabled })} />
            </div>
            <div className="settings-section grid gap-3 lg:grid-cols-12">
                <Form.Item label="ComfyUI 地址" className="mb-0 lg:col-span-8" extra="原生 ComfyUI 服务地址，默认本机 8188 端口。">
                    <Input
                        value={comfyui.baseUrl}
                        placeholder={fallbackBaseUrl}
                        onChange={(event) => update({ baseUrl: event.target.value })}
                        onBlur={() => update({ baseUrl: (comfyui.baseUrl.trim() || fallbackBaseUrl).replace(/\/+$/, "") })}
                    />
                </Form.Item>
                <Form.Item label="工作流用途" className="mb-0 lg:col-span-4">
                    <SegmentedControl block value={comfyui.capability} options={capabilityOptions} onChange={(value) => updateCapability(value as ComfyUICapability)} />
                </Form.Item>
                <Alert
                    className="lg:col-span-12"
                    type="info"
                    showIcon
                    title="本机地址需要显式放行"
                    description="后端默认拒绝本机与私网上游。请用 CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS=127.0.0.1 启动后端；该白名单只按主机名匹配，端口不参与判断。"
                />
                <Form.Item label="已保存条目" className="mb-0 lg:col-span-6" extra="选中后，画布工作流节点会使用该工作流的字段映射。">
                    <Select
                        className="w-full"
                        allowClear
                        value={comfyui.workflowId || undefined}
                        placeholder="尚未保存工作流"
                        options={comfyui.workflows.map((item) => ({
                            label: `${item.title || item.workflowId} · ${capabilityLabel(normalizeComfyUICapability(item.capability, comfyui.capability))}`,
                            value: item.workflowId,
                        }))}
                        onChange={selectWorkflow}
                    />
                </Form.Item>
                <Form.Item label="显示名称" className="mb-0 lg:col-span-6" extra="用于在画布工作流节点上区分不同工作流。">
                    <Input value={title} placeholder="例如：Flux 文生图" onChange={(event) => setTitle(event.target.value)} onBlur={updateTitle} />
                </Form.Item>
                <Form.Item label="操作" className="workflow-entry-actions-field mb-0 lg:col-span-12">
                    <div className="workflow-entry-actions">
                        <Button icon={<RefreshCw className="size-4" />} loading={parsing} onClick={() => void parseWorkflow()}>
                            解析并补全字段
                        </Button>
                        <Button type="primary" icon={<Plus className="size-4" />} disabled={!draftJson} onClick={saveWorkflow}>
                            保存为新工作流
                        </Button>
                        <Popconfirm
                            title="删除当前条目？"
                            description={selected ? `${selected.title || selected.workflowId}` : undefined}
                            okText="删除"
                            cancelText="取消"
                            onConfirm={() => selected && removeWorkflow(selected)}
                        >
                            <Button danger icon={<Trash2 className="size-4" />} disabled={!selected}>
                                删除
                            </Button>
                        </Popconfirm>
                    </div>
                </Form.Item>
                <details className="workflow-json-details lg:col-span-12">
                    <summary>查看或编辑 ComfyUI API JSON</summary>
                    <Form.Item className="mb-0 mt-3" extra="在 ComfyUI 里使用「导出（API 格式）」得到 JSON，粘贴后点击“解析并补全字段”读取节点枚举与数值范围；修改后移出输入框即保存。">
                        <Input.TextArea
                            rows={10}
                            value={jsonText}
                            spellCheck={false}
                            placeholder={'{"3":{"class_type":"...","inputs":{}}}'}
                            onChange={(event) => setJsonText(event.target.value)}
                            onBlur={() => updateWorkflowJson(jsonText)}
                        />
                    </Form.Item>
                </details>
                <div className="lg:col-span-12">
                    <WorkflowGraphEditor
                        workflowJson={editorJson}
                        fields={editorFields}
                        onChange={updateFields}
                        emptyDescription={selected ? "该条目没有可展示的工作流拓扑，请重新粘贴 API 格式 JSON 并解析" : "请先粘贴 API 格式工作流，再点击“解析并补全字段”"}
                    />
                </div>
            </div>
        </Form>
    );
}

function parseJsonObject(value: string): ComfyUIWorkflowJson | null {
    try {
        const parsed: unknown = JSON.parse(value);
        if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return null;
        return parsed as ComfyUIWorkflowJson;
    } catch {
        return null;
    }
}

function errorText(error: unknown) {
    return error instanceof Error ? error.message : String(error);
}
