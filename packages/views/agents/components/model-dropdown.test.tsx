// @vitest-environment jsdom

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, cleanup } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { I18nProvider } from "@multica/core/i18n/react";
import type { RuntimeModelsResult } from "@multica/core/types";
import { afterEach, describe, expect, it, vi } from "vitest";
import enAgents from "../../locales/en/agents.json";
import enCommon from "../../locales/en/common.json";
import enIssues from "../../locales/en/issues.json";
import { ModelDropdown } from "./model-dropdown";

const TEST_RESOURCES = {
  en: { common: enCommon, agents: enAgents, issues: enIssues },
};

const CODEX_MODELS: RuntimeModelsResult = {
  models: [
    {
      id: "gpt-5.6-sol",
      label: "GPT-5.6 Sol",
      provider: "openai",
      default: true,
    },
    { id: "gpt-5.6-terra", label: "GPT-5.6 Terra", provider: "openai" },
    { id: "gpt-5.6-luna", label: "GPT-5.6 Luna", provider: "openai" },
  ],
  supported: true,
};

// Discovery outcome for the next render. resolveRuntimeModels rejects with the
// daemon's reported error text, so a failure is modelled as a throwing queryFn.
let discovery: () => Promise<RuntimeModelsResult> = async () => CODEX_MODELS;
const mockRefreshRuntimeModels = vi.hoisted(() => vi.fn());

vi.mock("@multica/core/runtimes", () => ({
  runtimeModelsOptions: (runtimeId: string | null) => ({
    enabled: Boolean(runtimeId),
    queryKey: ["runtime-models", runtimeId, discoveryKey],
    queryFn: () => discovery(),
  }),
  refreshRuntimeModels: (...args: unknown[]) =>
    mockRefreshRuntimeModels(...args),
}));

// Bumped per test so React Query cannot serve a previous case's cached result.
let discoveryKey = 0;

function renderDropdown(runtimeProvider?: string) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const onChange = vi.fn();
  const view = render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <QueryClientProvider client={queryClient}>
        <ModelDropdown
          runtimeId="rt-codex"
          runtimeProvider={runtimeProvider}
          runtimeOnline
          value=""
          onChange={onChange}
        />
      </QueryClientProvider>
    </I18nProvider>,
  );
  return { ...view, onChange };
}

describe("ModelDropdown", () => {
  it("keeps native model selection disabled when deployment configuration is unavailable", async () => {
    discovery = async () => { throw new Error("native model is not configured"); };
    const { container } = renderDropdown("native");
    expect(container.querySelector('[data-slot="popover-trigger"]')).toBeNull();
    expect(await screen.findByText(enAgents.model_dropdown.deployment_unavailable)).toBeTruthy();
    expect(container.querySelector('input')).toBeNull();
  });

  it("shows a deployment-managed model without offering arbitrary model entry", async () => {
    discovery = async () => ({
      supported: false,
      models: [{ id: "deployment-model", label: "Deployment Model", default: true }],
    });
    const { container, onChange } = renderDropdown();
    expect(await screen.findByText("Deployment Model")).toBeTruthy();
    expect(container.querySelector('[data-slot="popover-trigger"]')).toBeNull();
    expect(onChange).not.toHaveBeenCalled();
  });

  it("lets a native agent select a configured workspace model", async () => {
    const { onChange } = renderDropdown("native");
    const trigger = await screen.findByRole("combobox");
    await vi.waitFor(() => expect(trigger).not.toBeDisabled());
    await userEvent.click(trigger);
    await userEvent.click(await screen.findByRole("option", { name: "GPT-5.6 Terra" }));
    expect(onChange).toHaveBeenCalledWith("gpt-5.6-terra");
  });

  afterEach(() => {
    cleanup();
    discovery = async () => CODEX_MODELS;
    mockRefreshRuntimeModels.mockReset();
    discoveryKey += 1;
  });

});
