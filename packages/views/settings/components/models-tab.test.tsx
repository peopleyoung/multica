import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "@multica/core/api";
import { renderWithI18n } from "../../test/i18n";
import type { WorkspaceModel } from "@multica/core/types/workspace-model";
import { ModelsTab } from "./models-tab";

const state = vi.hoisted(() => ({ role: "owner" }));
vi.mock("@multica/core/paths", () => ({
  useCurrentWorkspace: () => ({ id: "ws-1", name: "Acme" }),
}));
vi.mock("@multica/core/permissions", () => ({
  useCurrentMember: () => ({ role: state.role }),
}));
vi.mock("@multica/core/api", () => ({
  api: {
    listWorkspaceModels: vi.fn(),
    saveWorkspaceModel: vi.fn(),
    deleteWorkspaceModel: vi.fn(),
    testWorkspaceModel: vi.fn(),
  },
}));
const model: WorkspaceModel = {
  id: "model-1",
  name: "First model",
  base_url: "https://example.test/v1",
  model: "upstream-a",
  has_api_key: true,
  enabled: true,
  is_default: true,
  capabilities: {
    streaming_tool_calls: true,
    image_input: false,
    json_object: false,
    context_tokens: 32000,
    max_output_tokens: 1000,
    max_tool_argument_bytes: 4096,
    max_tools: 32,
  },
};
function renderModels() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return renderWithI18n(
    <QueryClientProvider client={client}>
      <ModelsTab />
    </QueryClientProvider>,
  );
}
beforeEach(() => {
  state.role = "owner";
  vi.mocked(api.listWorkspaceModels).mockResolvedValue([model]);
});
afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});

describe("ModelsTab", () => {
  it("preserves an existing credential when saving and keeps the draft after failure", async () => {
    vi.mocked(api.saveWorkspaceModel)
      .mockRejectedValueOnce(new Error("Save failed; retry"))
      .mockResolvedValueOnce(model);
    renderModels();
    await userEvent.click(
      await screen.findByRole("button", { name: "Edit First model" }),
    );
    const password = screen.getByLabelText("API key (optional)");
    expect(password).toHaveValue("");
    const name = screen.getByLabelText("Display name");
    await userEvent.clear(name);
    await userEvent.type(name, "Updated name");
    await userEvent.click(
      screen.getByRole("button", { name: "Save" }),
    );
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Save failed; retry",
    );
    expect(name).toHaveValue("Updated name");
    const input = vi.mocked(api.saveWorkspaceModel).mock.calls[0]?.[1];
    expect(input).toBeDefined();
    expect(input).not.toHaveProperty("api_key");
    await userEvent.click(
      screen.getByRole("button", { name: "Save" }),
    );
    await waitFor(() =>
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument(),
    );
    expect(api.saveWorkspaceModel).toHaveBeenLastCalledWith(
      "ws-1",
      expect.objectContaining({ name: "Updated name" }),
      "model-1",
    );
  });

  it("saves the selected API protocol without replacing credentials", async () => {
    vi.mocked(api.saveWorkspaceModel).mockResolvedValue(model);
    renderModels();
    await userEvent.click(await screen.findByRole("button", { name: "Edit First model" }));
    await userEvent.click(screen.getByRole("combobox", { name: "API protocol" }));
    await userEvent.click(await screen.findByRole("option", { name: "Responses" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(api.saveWorkspaceModel).toHaveBeenCalledWith("ws-1", expect.objectContaining({ capabilities: expect.objectContaining({ api: "responses" }) }), "model-1"));
    expect(vi.mocked(api.saveWorkspaceModel).mock.calls[0]?.[1]).not.toHaveProperty("api_key");
  });

  it("shows workspace models to members without credential management controls", async () => {
    state.role = "member";
    renderModels();
    expect(await screen.findByText("First model")).toBeVisible();
    expect(
      screen.queryByRole("button", { name: /Edit First model/ }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /Add model/ }),
    ).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Test connection/ })).not.toBeInTheDocument();
  });

  it("tests a saved model and displays the tool check result without saving", async () => {
    let resolve!: (result: { ok: boolean; stage: "complete"; code: string; duration_ms: number }) => void;
    vi.mocked(api.testWorkspaceModel).mockImplementation(() => new Promise((done) => { resolve = done; }));
    renderModels();
    const button = await screen.findByRole("button", { name: "Test connection First model" });
    await userEvent.click(button);
    expect(button).toBeDisabled();
    expect(button).toHaveAttribute("aria-busy", "true");
    expect(api.testWorkspaceModel).toHaveBeenCalledWith("ws-1", expect.objectContaining({ model: "upstream-a" }), "model-1", expect.any(AbortSignal));
    expect(vi.mocked(api.testWorkspaceModel).mock.calls[0]?.[1]).not.toHaveProperty("api_key");
    await act(async () => resolve({ ok: true, stage: "complete", code: "ok", duration_ms: 123 }));
    expect(await screen.findByText("Text and tool calls passed (123 ms).")).toBeVisible();
    expect(api.saveWorkspaceModel).not.toHaveBeenCalled();
  });

  it("uses draft protocol and replacement key, cancels stale tests, and never saves", async () => {
    let resolve!: (result: { ok: boolean; stage: "complete"; code: string; duration_ms: number }) => void;
    vi.mocked(api.testWorkspaceModel).mockImplementation(() => new Promise((done) => { resolve = done; }));
    renderModels();
    await userEvent.click(await screen.findByRole("button", { name: "Edit First model" }));
    const dialog = within(screen.getByRole("dialog"));
    await userEvent.click(dialog.getByRole("combobox", { name: "API protocol" }));
    await userEvent.click(await screen.findByRole("option", { name: "Responses" }));
    await userEvent.type(dialog.getByLabelText("API key (optional)"), "draft-key");
    await userEvent.click(dialog.getByRole("button", { name: "Test connection" }));
    expect(api.testWorkspaceModel).toHaveBeenCalledWith("ws-1", expect.objectContaining({ api_key: "draft-key", capabilities: expect.objectContaining({ api: "responses" }) }), "model-1", expect.any(AbortSignal));
    const signal = vi.mocked(api.testWorkspaceModel).mock.calls[0]?.[3];
    await userEvent.type(dialog.getByLabelText("Model ID"), "-changed");
    expect(signal?.aborted).toBe(true);
    await act(async () => resolve({ ok: true, stage: "complete", code: "ok", duration_ms: 123 }));
    expect(screen.queryByText("Text and tool calls passed (123 ms).")).not.toBeInTheDocument();
    expect(api.saveWorkspaceModel).not.toHaveBeenCalled();
  });

  it("allows a new model to be tested before saving or checking tool support", async () => {
    vi.mocked(api.testWorkspaceModel).mockResolvedValue({ ok: false, stage: "tool_call", code: "model_request_rejected", duration_ms: 45 });
    renderModels();
    await userEvent.click(await screen.findByRole("button", { name: "Add model" }));
    const dialog = within(screen.getByRole("dialog"));
    await userEvent.type(dialog.getByLabelText("OpenAI-compatible API base URL"), "https://draft.example.test/v1");
    await userEvent.type(dialog.getByLabelText("Model ID"), "new-model");
    await userEvent.click(dialog.getByRole("button", { name: "Test connection" }));
    expect(await dialog.findByRole("alert")).toHaveTextContent("Tool call: The API rejected this request.");
    expect(api.testWorkspaceModel).toHaveBeenCalledWith("ws-1", expect.objectContaining({ model: "new-model", capabilities: expect.objectContaining({ streaming_tool_calls: false }) }), undefined, expect.any(AbortSignal));
    expect(api.saveWorkspaceModel).not.toHaveBeenCalled();
  });

  it("tests removal of a stored credential and clears results when closing", async () => {
    vi.mocked(api.testWorkspaceModel).mockResolvedValue({ ok: true, stage: "complete", code: "ok", duration_ms: 12 });
    renderModels();
    await userEvent.click(await screen.findByRole("button", { name: "Edit First model" }));
    const dialog = within(screen.getByRole("dialog"));
    await userEvent.click(dialog.getByLabelText("Remove saved key"));
    await userEvent.click(dialog.getByRole("button", { name: "Test connection" }));
    expect(await dialog.findByText("Text and tool calls passed (12 ms).")).toBeVisible();
    expect(api.testWorkspaceModel).toHaveBeenCalledWith("ws-1", expect.objectContaining({ api_key: "" }), "model-1", expect.any(AbortSignal));
    await userEvent.click(dialog.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await userEvent.click(screen.getByRole("button", { name: "Edit First model" }));
    expect(screen.queryByText("Text and tool calls passed (12 ms).")).not.toBeInTheDocument();
  });

  it("shows a localized request failure without exposing raw errors or retrying", async () => {
    vi.mocked(api.testWorkspaceModel).mockRejectedValue(new Error("private-upstream-detail"));
    renderModels();
    await userEvent.click(await screen.findByRole("button", { name: "Test connection First model" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Connection test failed.");
    expect(screen.queryByText(/private-upstream-detail/)).not.toBeInTheDocument();
    expect(api.testWorkspaceModel).toHaveBeenCalledOnce();
    expect(screen.getByRole("button", { name: "Test connection First model" })).toBeEnabled();
  });

  it("cancels an in-flight test when its editor closes", async () => {
    let resolve!: (result: { ok: boolean; stage: "complete"; code: string; duration_ms: number }) => void;
    vi.mocked(api.testWorkspaceModel).mockImplementation(() => new Promise((done) => { resolve = done; }));
    renderModels();
    await userEvent.click(await screen.findByRole("button", { name: "Edit First model" }));
    const dialog = within(screen.getByRole("dialog"));
    await userEvent.click(dialog.getByRole("button", { name: "Test connection" }));
    const signal = vi.mocked(api.testWorkspaceModel).mock.calls[0]?.[3];
    await userEvent.click(dialog.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(signal?.aborted).toBe(true);
    await act(async () => resolve({ ok: true, stage: "complete", code: "ok", duration_ms: 12 }));
    await userEvent.click(screen.getByRole("button", { name: "Edit First model" }));
    expect(screen.queryByText("Text and tool calls passed (12 ms).")).not.toBeInTheDocument();
  });
});
