// @vitest-environment node
import { describe, expect, it, vi } from "vitest";
import { QueryClient } from "@tanstack/react-query";
import { nativeModelOptions } from "./native-model";
const nativeModel = vi.hoisted(() => vi.fn());
vi.mock("@/data/api", () => ({ api: { nativeModel } }));

describe("nativeModelOptions", () => {
  it("keeps workspace caches separate and forwards cancellation", async () => {
    nativeModel.mockResolvedValue({ status: "completed", supported: false, models: [{ id: "fixed", label: "Fixed model", default: true }] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    expect(await client.fetchQuery(nativeModelOptions("ws-1", "runtime-1"))).toBe("Fixed model");
    expect(nativeModel).toHaveBeenCalledWith("runtime-1", { signal: expect.any(AbortSignal) });
    expect(client.getQueryData(nativeModelOptions("ws-2", "runtime-1").queryKey)).toBeUndefined();
    client.clear();
  });
  it.each([
    { status: "failed", error: "configuration missing" },
    { status: "pending", supported: false },
    { status: "completed", supported: true, models: [{ id: "arbitrary", default: true }] },
    { status: "completed", supported: false, models: [] },
  ])("rejects unavailable or editable model responses: %j", async (response) => {
    nativeModel.mockResolvedValue(response);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    await expect(client.fetchQuery(nativeModelOptions("ws", "runtime"))).rejects.toThrow();
    client.clear();
  });
});
