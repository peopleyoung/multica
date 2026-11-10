// @vitest-environment jsdom
import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { Agent, AgentRuntime } from "@multica/core/types";
import { I18nProvider } from "@multica/core/i18n/react";
import enAgents from "../../../locales/en/agents.json";
import enCommon from "../../../locales/en/common.json";
import { NativeRuntimeConfigTab } from "./native-runtime-config-tab";

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
const agent: Agent = {
  id: "agent",
  workspace_id: "ws",
  runtime_id: "runtime",
  name: "Agent",
  description: "",
  instructions: "",
  avatar_url: null,
  runtime_mode: "local",
  runtime_config: {},
  custom_args: [],
  visibility: "workspace",
  permission_mode: "public_to",
  invocation_targets: [],
  status: "idle",
  max_concurrent_tasks: 1,
  model: "",
  owner_id: "owner",
  skills: [],
  created_at: "2026-11-09T00:00:00Z",
  updated_at: "2026-11-09T00:00:00Z",
  archived_at: null,
  archived_by: null,
};
const runtime: AgentRuntime = {
  id: "runtime",
  workspace_id: "ws",
  daemon_id: "daemon",
  name: "Native",
  runtime_mode: "local",
  provider: "native",
  launch_header: "",
  status: "online",
  device_info: "",
  metadata: {
    native_limits: {
      write_files: true,
      shell: true,
      http_origins: ["https://cvat.example"],
    },
  },
  owner_id: "owner",
  visibility: "private",
  last_seen_at: null,
  created_at: "",
  updated_at: "",
};
function setup(
  canConfigure = true,
  target = runtime,
  onSave = vi.fn().mockResolvedValue(undefined),
) {
  render(
    <I18nProvider
      locale="en"
      resources={{ en: { agents: enAgents, common: enCommon } }}
    >
      <NativeRuntimeConfigTab
        agent={agent}
        runtime={target}
        canConfigure={canConfigure}
        onSave={onSave}
      />
    </I18nProvider>,
  );
  return onSave;
}
describe("NativeRuntimeConfigTab", () => {
  it("saves effective execution options and a service credential reference", async () => {
    const user = userEvent.setup();
    const onSave = setup();
    await user.click(screen.getByRole("switch", { name: /write files/i }));
    await user.click(screen.getByRole("button", { name: /add service/i }));
    await user.type(screen.getByLabelText("Service name"), "cvat");
    await user.type(
      screen.getByLabelText("API base URL"),
      "https://cvat.example/api",
    );
    await user.type(
      screen.getByLabelText("Credential environment variable"),
      "CVAT_TOKEN",
    );
    await user.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() =>
      expect(onSave).toHaveBeenCalledWith({
        runtime_config: {
          write_files: true,
          shell: false,
          services: [
            {
              name: "cvat",
              base_url: "https://cvat.example/api",
              token_env: "CVAT_TOKEN",
              auth_scheme: "Token",
              allow_write: false,
            },
          ],
        },
      }),
    );
  });
  it("does not offer host powers when limits are missing or the actor is not the owner", () => {
    setup(false, { ...runtime, metadata: {} });
    expect(
      screen.getByRole("switch", { name: /run scripts/i }),
    ).toHaveAttribute("aria-disabled", "true");
    expect(
      screen.getByRole("switch", { name: /write files/i }),
    ).toHaveAttribute("aria-disabled", "true");
    expect(
      screen.queryByRole("button", { name: /^save$/i }),
    ).not.toBeInTheDocument();
  });
  it("keeps the edited settings after a failed save", async () => {
    const user = userEvent.setup();
    const onSave = vi.fn().mockRejectedValue(new Error("unavailable"));
    setup(true, runtime, onSave);
    await user.click(screen.getByRole("switch", { name: /write files/i }));
    await user.click(screen.getByRole("button", { name: /^save$/i }));
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /^save$/i })).toBeEnabled(),
    );
    expect(screen.getByRole("switch", { name: /write files/i })).toBeChecked();
  });
});


it("saves a remote execution environment shared by files and commands", async () => {
  const user = userEvent.setup();
  const onSave = setup();
  await user.click(screen.getByRole("button", { name: /add service/i }));
  await user.type(screen.getByLabelText("Service name"), "multica_jobs");
  await user.type(screen.getByLabelText("API base URL"), "https://cvat.example");
  await user.click(screen.getByRole("switch", { name: /remote execution environment/i }));
  await user.click(screen.getByRole("button", { name: /^save$/i }));
  await waitFor(() => expect(onSave).toHaveBeenCalledWith({ runtime_config: expect.objectContaining({ environment: "multica_jobs" }) }));
});
