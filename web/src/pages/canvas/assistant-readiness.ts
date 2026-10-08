import { getAgentHostStatus, type AgentHostStatus } from "@/services/api/agent-assistant";
import { assistantStatusNotice } from "./canvas-assistant-copy";

/** Only wait before dispatch. Never replay a chat whose outcome might already include writes. */
// canvasId 只用于让后端带上会话级提示（sessionWarning）；available 判定不受它影响。
export async function waitForAssistant(
    signal: AbortSignal,
    readStatus: (canvasId?: string) => Promise<AgentHostStatus> = getAgentHostStatus,
    canvasId?: string,
    attempts = 20,
): Promise<AgentHostStatus> {
    for (let attempt = 0; attempt < attempts; attempt++) {
        signal.throwIfAborted();
        const status = await readStatus(canvasId);
        signal.throwIfAborted();
        if (status.available) return status;
        if (status.reason !== "host_starting") throw new Error(assistantStatusNotice(status.reason).text);
        await new Promise<void>((resolve, reject) => {
            const abort = () => { clearTimeout(timer); reject(new DOMException("Stopped", "AbortError")); };
            const timer = setTimeout(() => { signal.removeEventListener("abort", abort); resolve(); }, 500);
            signal.addEventListener("abort", abort, { once: true });
        });
    }
    throw new Error("连接暂时未就绪，你的消息还在，请稍后重试");
}
