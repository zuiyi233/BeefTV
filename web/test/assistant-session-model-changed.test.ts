import { describe, expect, test } from "bun:test";

import { AgentTurnFailedError, agentAssistantFailureText } from "@/services/api/agent-assistant";
import { ASSISTANT_SESSION_MODEL_CHANGED, assistantSessionNotice, assistantTurnCanContinue } from "@/pages/canvas/canvas-assistant-copy";
import { waitForAssistant } from "@/pages/canvas/assistant-readiness";

/** 与后端 assistant.SessionModelChangedMessage 同一口径；改这里要同时改后端常量。 */
const SESSION_MODEL_CHANGED_TEXT = "这一轮开始时用的是另一个模型，创作助手没有执行它。现在已改用当前模型，重新发送这条要求即可继续；也可以新建对话，历史仍然可以查看。";

describe("会话级换模型原因有独立可行动文案", () => {
    test("按 reason 分支给出与后端一致的说明，不落到通用兜底", () => {
        expect(agentAssistantFailureText(ASSISTANT_SESSION_MODEL_CHANGED)).toBe(SESSION_MODEL_CHANGED_TEXT);
        expect(agentAssistantFailureText(ASSISTANT_SESSION_MODEL_CHANGED)).not.toBe("创作助手暂时不可用，请稍后再试");
        expect(agentAssistantFailureText(ASSISTANT_SESSION_MODEL_CHANGED)).not.toBe("助手暂时用不了，请稍后再试");
        // 说清出路（新建对话）与不丢历史，且不把机器可读值露给用户。
        expect(agentAssistantFailureText(ASSISTANT_SESSION_MODEL_CHANGED)).toContain("新建对话");
        expect(agentAssistantFailureText(ASSISTANT_SESSION_MODEL_CHANGED)).toContain("历史");
        expect(agentAssistantFailureText(ASSISTANT_SESSION_MODEL_CHANGED)).not.toContain(ASSISTANT_SESSION_MODEL_CHANGED);
    });

    test("这条原因结束的回合也带同一句话，但不提供同会话继续入口", () => {
        const error = new AgentTurnFailedError(ASSISTANT_SESSION_MODEL_CHANGED);
        expect(error.message).toBe(SESSION_MODEL_CHANGED_TEXT);
        // 继续提示会在同一条对话里重发，与「请新建对话」自相矛盾，所以必须留在不可继续。
        expect(assistantTurnCanContinue(ASSISTANT_SESSION_MODEL_CHANGED)).toBe(false);
    });
});

describe("会话级提示只在宿主可用时出现，且不接管不可用提示", () => {
    test("available:true 且带这条原因才给出提示和新对话出路", () => {
        const notice = assistantSessionNotice({ available: true, sessionWarning: ASSISTANT_SESSION_MODEL_CHANGED });
        expect(notice).not.toBeNull();
        expect(notice?.text).toBe(SESSION_MODEL_CHANGED_TEXT);
        expect(notice?.actionLabel).toBe("新建对话");
    });

    test("没有提示、提示值不认识、宿主不可用都不出这条提示", () => {
        expect(assistantSessionNotice({ available: true })).toBeNull();
        expect(assistantSessionNotice({ available: true, sessionWarning: "something_new_from_backend" })).toBeNull();
        // 宿主已经不可用时由上面的阻断提示处理，两条同时出现会互相矛盾。
        expect(assistantSessionNotice({ available: false, reason: "host_unreachable", sessionWarning: ASSISTANT_SESSION_MODEL_CHANGED })).toBeNull();
        expect(assistantSessionNotice(null)).toBeNull();
        expect(assistantSessionNotice(undefined)).toBeNull();
    });
});

describe("发送前的就绪检查按画布读状态，会话级提示不拦下这一轮", () => {
    test("canvasId 透传给状态读取，available 仍为 true 就直接放行", async () => {
        const seen: (string | undefined)[] = [];
        const status = await waitForAssistant(
            new AbortController().signal,
            async (canvasId) => { seen.push(canvasId); return { available: true, sessionWarning: ASSISTANT_SESSION_MODEL_CHANGED }; },
            "canvas-a",
        );
        expect(seen).toEqual(["canvas-a"]);
        expect(status.available).toBe(true);
        // 提示字段必须活着回到界面，否则发送后会被不带画布的状态覆盖掉。
        expect(status.sessionWarning).toBe(ASSISTANT_SESSION_MODEL_CHANGED);
    });
});
