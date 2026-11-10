// @vitest-environment node

import { describe, expect, it } from "vitest";
import { AgentSchema, AgentTaskSchema, RuntimeSchema } from "./schemas";

describe("native runtime response compatibility", () => {
  it("keeps the native provider and additive deployment metadata", () => {
    const runtime = RuntimeSchema.parse({
      id: "runtime-native",
      provider: "native",
      name: "Multica Native",
      status: "online",
      metadata: { model: "deployment-model" },
      future_capability: true,
    });
    expect(runtime.provider).toBe("native");
    expect(runtime.metadata).toEqual({ model: "deployment-model" });
    expect(runtime.status).toBe("online");
  });

  it("parses a native agent and its completed run without CLI-specific fields", () => {
    const agent = AgentSchema.parse({ id: "agent-native", runtime_id: "runtime-native", model: "" });
    const run = AgentTaskSchema.parse({
      id: "run-native", agent_id: agent.id, runtime_id: agent.runtime_id,
      status: "completed", session_id: "native-session", result: "Updated.",
    });
    expect(agent.runtime_id).toBe("runtime-native");
    expect(agent.model).toBe("");
    expect(run.status).toBe("completed");
    expect(run.result).toBe("Updated.");
  });

  it("degrades malformed presence fields while retaining native identity", () => {
    const runtime = RuntimeSchema.parse({ id: "runtime-native", provider: "native", status: "new-server-status" });
    expect(runtime.provider).toBe("native");
    expect(runtime.status).toBe("offline");
  });
});
