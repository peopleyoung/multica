// @vitest-environment jsdom

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { I18nProvider } from "@multica/core/i18n/react";
import type { RuntimeModelsResult } from "@multica/core/types";
import { afterEach, describe, expect, it, vi } from "vitest";
import enAgents from "../../../locales/en/agents.json";
import enCommon from "../../../locales/en/common.json";
import enIssues from "../../../locales/en/issues.json";
import { ModelPicker } from "./model-picker";

const TEST_RESOURCES = {
  en: { common: enCommon, agents: enAgents, issues: enIssues },
};

// MUL-6961. The create-agent dropdown and this inspector picker are two
// separate renderers over one catalog, and the first attempt at unavailable
// models only taught the dropdown about them — leaving the inspector, which is
// where an ALREADY configured agent gets edited, still offering a model whose
// every run 400s. That asymmetry is what this file exists to prevent.
const CLAUDE_CATALOG: RuntimeModelsResult = {
  models: [{ id: "claude-fable-5", label: "Fable", provider: "anthropic" }],
  unavailableModels: [
    {
      id: "cc-update-required-1",
      label: "Fable 5.1 (disabled)",
      reason: "Update to 2.1.255+ to use Fable 5.1",
    },
  ],
  supported: true,
};

let discovery: () => Promise<RuntimeModelsResult> = async () => CLAUDE_CATALOG;
let discoveryKey = 0;
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

function renderPicker(runtimeProvider?: string) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const onChange = vi.fn(async () => {});
  const view = render(
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      <QueryClientProvider client={queryClient}>
        <ModelPicker
          runtimeId="rt-claude"
          runtimeProvider={runtimeProvider}
          runtimeOnline
          value=""
          onChange={onChange}
        />
      </QueryClientProvider>
    </I18nProvider>,
  );
  return { ...view, onChange, queryClient };
}

describe("ModelPicker (inspector)", () => {
  it("keeps native configuration failures visible without manual model entry", async () => {
    discovery = async () => { throw new Error("native model is not configured"); };
    const { container } = renderPicker("native");
    expect(await screen.findByText(enAgents.model_dropdown.deployment_unavailable)).toBeTruthy();
    expect(container.querySelector('button, input')).toBeNull();
  });

  afterEach(() => {
    cleanup();
    discovery = async () => CLAUDE_CATALOG;
    discoveryKey += 1;
    mockRefreshRuntimeModels.mockReset();
  });

});
