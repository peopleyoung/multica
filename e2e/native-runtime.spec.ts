import { test, expect } from "@playwright/test";
import { spawn, type ChildProcess } from "node:child_process";
import { once } from "node:events";
import { createServer } from "node:http";
import { mkdir, mkdtemp, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { homedir, tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { TestApiClient } from "./fixtures";

test.use({ timezoneId: "Asia/Shanghai" });

// Run against an API configured with this fake endpoint and fixed model:
// MULTICA_LLM_BASE_URL=http://127.0.0.1:25440/v1
// MULTICA_LLM_DEFAULT_MODEL=native-browser-fixture
// Native capabilities must be enabled. The binary is an explicit test build,
// never a user-installed executable. No real model or AI CLI is invoked.
test("native runtime supports file chat, Agent Builder, Mika and captured-source images", async ({ page }, testInfo) => {
  test.skip(!process.env.MULTICA_NATIVE_E2E_BINARY, "Requires an explicit Multica test binary and local fake-model deployment");
  test.setTimeout(360_000);
  page.setDefaultTimeout(20_000);
  page.setDefaultNavigationTimeout(90_000);
  const api = new TestApiClient();
  const id = `native-e2e-${Date.now().toString(36)}-${process.pid}`;
  const scratch = await mkdtemp(join(tmpdir(), "multica-native-e2e-"));
  const profileDir = join(homedir(), ".multica", "profiles", id);
  const modelRequests: Array<{ model: string; messages: Array<{ role: string; content?: string }> }> = [];
  let continued = false;
  let builderRequests = 0;
  let mikaAgentId = "";
  let mikaSessionId = "";
  let mikaProjectId = "";
  let mikaIssueId = "";
  let mikaLoadedSkill = false;
  let mikaKickoffSeen = false;
  let mikaWrites = 0;
  let digestId = "";
  let digestWrites = 0;
  let digestRetried = false;
  let digestHistoryRecovered = false;
  let capturedIssueId = "";
  let capturedWrites = 0;
  let capturedImageSeen = false;
  const captureImage = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aN1sAAAAASUVORK5CYII=", "base64");
  const modelErrors: string[] = [];
  let daemon: ChildProcess | undefined;
  let workspaceCreated = false;
  const upstream = createServer(async (req, res) => {
    try {
      let raw = "";
      for await (const chunk of req) raw += chunk;
      const body = JSON.parse(raw);
      modelRequests.push(body);
      if (!body.stream) {
        res.setHeader("Content-Type", "application/json");
        res.end(JSON.stringify({ choices: [{ index: 0, message: { role: "assistant", content: "Native acceptance" }, finish_reason: "stop" }], usage: { prompt_tokens: 100, completion_tokens: 20 } }));
        return;
      }
      const tools = body.messages.filter((m: { role: string }) => m.role === "tool");
      const lastUser = body.messages.filter((m: { role: string }) => m.role === "user").at(-1);
      const lastUserText = typeof lastUser?.content === "string"
        ? lastUser.content
        : (lastUser?.content ?? []).filter((part: { type: string }) => part.type === "text").map((part: { text: string }) => part.text).join("\n");
      const turnTools = body.messages.slice(body.messages.lastIndexOf(lastUser) + 1).filter((message: { role: string }) => message.role === "tool");
      const toolCall = (name: string, args: Record<string, unknown>) => ({ name, arguments: JSON.stringify(args) });
      let call: { name: string; arguments: string } | undefined;
      let content = "Native file updated.";
      if (lastUserText.includes('"kind":"quick_create"') && lastUserText.includes("Native capture request")) {
        capturedImageSeen = Array.isArray(lastUser.content) && lastUser.content.some((part: { type: string; image_url?: { url: string } }) => part.type === "image_url" && part.image_url?.url === `data:image/png;base64,${captureImage.toString("base64")}`);
        if (!capturedImageSeen || !lastUserText.includes("Native captured historical facts")) throw new Error("Captured image or historical facts did not reach the model");
        if (turnTools.length === 0) {
          capturedWrites++;
          call = toolCall("multica_create_issue", { title: "Native captured follow-up", description: "Follow the captured historical facts and image.", status: "backlog" });
        } else {
          const issue = JSON.parse(turnTools[0].content);
          capturedIssueId = issue.id;
          content = `Created ${issue.identifier ?? issue.id}: Native captured follow-up.`;
        }
      } else if (body.messages.some((message: { content?: string }) => message.content?.includes("You are Multica Agent Builder."))) {
        builderRequests++;
        content = `Review this agent before creating it.\n<agent_draft>${JSON.stringify({
          name: "Native builder reviewer",
          description: "Reviews a bounded change.",
          instructions: "Review the requested change and report actionable findings. Ask before changing files.",
          conversation_starters: [{ label: "Review a change", prompt: "Review the current change." }],
          model: "",
          skill_ids: [],
          permission_scope: "private",
          member_ids: [],
        })}</agent_draft>`;
      } else if (lastUser?.content?.includes("Preview native onboarding goal")) {
        mikaKickoffSeen = body.messages.some((message: { content?: string }) => message.content?.includes("opening-already-sent") && message.content.includes("Member IANA timezone"));
        if (turnTools.length === 0) {
          call = toolCall("load_skill", { name: "multica-onboarding" });
        } else {
          mikaLoadedSkill = turnTools[0].content.includes("Preview and confirm");
          content = "Preview: create the Native onboarding project and one report issue assigned to Mika. Start it after confirmation?";
        }
      } else if (lastUser?.content?.includes("Confirm native onboarding plan")) {
        if (turnTools.length === 0) {
          mikaWrites++;
          call = toolCall("multica_manage_project", { action: "create", values: { title: "Native onboarding project", description: "A confirmed first outcome" } });
        } else if (turnTools.length === 1) {
          mikaProjectId = JSON.parse(turnTools[0].content).id;
          mikaWrites++;
          call = toolCall("multica_create_issue", { title: "Native onboarding report", description: "Produce a short acceptance report and post it on this issue. Leave it in_review for the member.", status: "todo", assignee_type: "agent", assignee_id: mikaAgentId, project_id: mikaProjectId });
        } else {
          const issue = JSON.parse(turnTools[1].content);
          mikaIssueId = issue.id;
          content = `Started ${issue.identifier ?? issue.id}, assigned to Mika. Progress and results live on the issue. You can open it or message me to change direction.`;
        }
      } else if (lastUser?.content?.includes("Set up a daily automation that posts a morning summary")) {
        content = "Preview: daily workspace digest at 09:00 Asia/Shanghai, assigned to Mika. Confirm to schedule it.";
      } else if (lastUser?.content?.includes("Confirm native digest schedule")) {
        if (turnTools.length === 0) {
          digestHistoryRecovered = body.messages.some((message: { content?: string }) => message.content === "Preview: daily workspace digest at 09:00 Asia/Shanghai, assigned to Mika. Confirm to schedule it.");
          if (!digestRetried) {
            digestRetried = true;
            res.writeHead(503).end();
            return;
          }
          digestWrites++;
          call = toolCall("multica_manage_autopilot", { action: "create", values: { title: "Native morning digest", description: "Summarize workspace progress each morning.", assignee_type: "agent", assignee_id: mikaAgentId, execution_mode: "run_only" } });
        } else if (turnTools.length === 1) {
          digestId = JSON.parse(turnTools[0].content).id;
          digestWrites++;
          call = toolCall("multica_manage_autopilot", { action: "add_trigger", id: digestId, values: { kind: "schedule", cron_expression: "0 9 * * *", timezone: "Asia/Shanghai" } });
        } else {
          content = "Scheduled your daily digest for 09:00 Asia/Shanghai. You can pause it in Automations.";
        }
      } else if (mikaAgentId && lastUser?.content?.includes('"kind":"issue"')) {
        const issueId = lastUser.content.match(/"issue_id":"([^"]+)"/)?.[1];
        if (!issueId) throw new Error("Missing assigned issue in native run context");
        if (turnTools.length === 0) {
          call = toolCall("multica_get_issue", { issue_id: issueId });
        } else if (turnTools.length === 1) {
          call = toolCall("multica_create_comment", { issue_id: issueId, content: "Native onboarding report: the confirmed task executed through the built-in runtime." });
        } else if (turnTools.length === 2) {
          call = toolCall("multica_get_issue", { issue_id: issueId });
        } else if (turnTools.length === 3) {
          call = toolCall("multica_update_issue_status", { issue_id: issueId, status: "in_review", expected_revision: JSON.parse(turnTools[2].content).revision });
        } else {
          content = "Native onboarding issue completed for review.";
        }
      } else if (lastUser?.content?.includes("remember")) {
        continued = tools.length === 3 && body.messages.some((m: { content?: string }) => m.content === "Native file updated.");
        content = "I remember the native file update.";
      } else if (tools.length === 0) {
        call = { name: "apply_patch", arguments: JSON.stringify({ path: "native-e2e.txt", old_text: "", new_text: "before" }) };
      } else if (tools.length === 1) {
        call = { name: "read_file", arguments: JSON.stringify({ path: "native-e2e.txt" }) };
      } else if (tools.length === 2) {
        call = { name: "apply_patch", arguments: JSON.stringify({ path: "native-e2e.txt", old_text: "before", new_text: "after" }) };
      }
      res.setHeader("Content-Type", "text/event-stream");
      // Report fixture usage so the shared quota can release conservative
      // reservations before the later image request reserves its context window.
      res.end(`data: ${JSON.stringify({ choices: [{ index: 0, delta: call ? { tool_calls: [{ index: 0, id: `native-${tools.length}`, type: "function", function: call }] } : { content }, finish_reason: call ? "tool_calls" : "stop" }] })}\n\ndata: ${JSON.stringify({ choices: [], usage: { prompt_tokens: 100, completion_tokens: 20 } })}\n\ndata: [DONE]\n\n`);
    } catch (error) {
      modelErrors.push(String(error));
      res.writeHead(500).end();
    }
  });
  try {
    upstream.listen(25440, "127.0.0.1");
    await once(upstream, "listening");
    await api.login(`${id}@multica.ai`, "Native E2E");
    const workspace = await api.ensureWorkspace(id, id);
    workspaceCreated = true;
    const token = api.getToken();
    if (!token) throw new Error("Missing test token");
    await mkdir(profileDir, { recursive: true, mode: 0o700 });
    await writeFile(join(profileDir, "config.json"), JSON.stringify({
      token, workspace_id: workspace.id,
      server_url: process.env.NEXT_PUBLIC_API_URL,
    }), { mode: 0o600 });
    // Remove inherited run/deployment credentials and explicitly fence every
    // registered AI command to a test-created missing executable.
    const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("MULTICA_") && key !== "OPENAI_API_KEY"));
    const commandNames = (await readFile(resolve("scripts/agent-cli-command-names.txt"), "utf8")).split("\n").filter((name) => name && !name.startsWith("#"));
    for (const name of commandNames) env[`MULTICA_${name.toUpperCase().replaceAll("-", "_")}_PATH`] = join(scratch, "missing", name);
    Object.assign(env, { MULTICA_NATIVE_RUNTIME: "true", MULTICA_NATIVE_ALLOW_WRITE: "true", MULTICA_GC_ENABLED: "false" });
    const launchDaemon = () => spawn(process.env.MULTICA_NATIVE_E2E_BINARY!, [
      "daemon", "start", "--foreground", "--profile", id,
      "--server-url", process.env.NEXT_PUBLIC_API_URL!,
      "--workspaces-root", join(scratch, "workspaces"),
      "--no-auto-update", "--no-auto-reload", "--poll-interval", "1s", "--heartbeat-interval", "2s",
    ], { cwd: scratch, env, stdio: "ignore" });
    daemon = launchDaemon();
    await expect.poll(async () => (await api.listRuntimes()).some((runtime) => runtime.provider === "native" && runtime.status === "online"), { timeout: 30_000 }).toBe(true);
    await page.addInitScript((value) => {
      localStorage.setItem("multica_token", value);
      localStorage.setItem("multica:chat:isOpen", "false");
    }, token);
    await page.goto("/onboarding", { waitUntil: "domcontentloaded" });
    await page.getByRole("button", { name: "Continue on web", exact: true }).click({ timeout: 60_000 });
    await page.getByRole("radio", { name: /Engineer \/ developer/i }).click();
    await page.getByRole("checkbox", { name: /Ship code with AI agents/i }).click();
    await page.getByRole("button", { name: "Continue", exact: true }).click();
    await page.getByRole("radio", { name: new RegExp(workspace.name) }).click();
    await page.getByRole("button", { name: `Open ${workspace.name}`, exact: true }).click();
    await page.getByRole("button", { name: "Show steps", exact: true }).click();
    await expect(page.getByText("native-browser-fixture", { exact: true })).toBeVisible({ timeout: 60_000 });
    const mikaCreated = page.waitForResponse((response) => response.url().endsWith("/api/agents/mika") && response.request().method() === "POST");
    await page.getByRole("button", { name: "Start with Mika", exact: true }).click();
    const mikaResponse = await mikaCreated;
    expect(mikaResponse.ok()).toBe(true);
    const mika = await mikaResponse.json();
    mikaAgentId = mika.id;
    mikaSessionId = mika.onboarding_session.id;
    expect(mika.model).toBe("");
    await expect(page).toHaveURL(new RegExp(`/chat\\?session=${mikaSessionId}`), { timeout: 60_000 });
    await expect(page.getByRole("button", { name: "Let the daily digest write itself", exact: true })).toBeVisible({ timeout: 60_000 });

    await page.goto(`/${workspace.slug}/agents/new/manual`, { waitUntil: "domcontentloaded" });
    await expect(page.getByText("native-browser-fixture", { exact: true })).toBeVisible({ timeout: 90_000 });
    await page.getByRole("textbox", { name: "Name", exact: true }).fill("Native browser agent");
    const created = page.waitForResponse((response) => response.url().endsWith("/api/agents") && response.request().method() === "POST");
    await page.getByRole("button", { name: "Create & open agent", exact: true }).click();
    const response = await created;
    expect(response.ok()).toBe(true);
    const agent = await response.json();
    expect(agent.model).toBe("");
    const session = await api.createChatSession(agent.id);
    await page.goto(`/${workspace.slug}/chat?session=${session.id}`, { waitUntil: "domcontentloaded" });
    const composer = page.locator('[contenteditable="true"]').last();
    await expect(composer).toBeVisible({ timeout: 60_000 });
    await composer.fill("Update the native fixture file.");
    await composer.press("Control+Enter");
    await expect(page.getByTestId("virtuoso-item-list").getByText("Native file updated.", { exact: true })).toBeVisible({ timeout: 40_000 });
    await composer.fill("Do you remember the change?");
    await composer.press("Control+Enter");
    await expect(page.getByTestId("virtuoso-item-list").getByText("I remember the native file update.", { exact: true })).toBeVisible({ timeout: 40_000 });
    expect(continued).toBe(true);
    expect(modelRequests.length).toBeGreaterThanOrEqual(5);
    expect(modelRequests.every((request) => request.model === "native-browser-fixture")).toBe(true);
    const entries = await readdir(join(scratch, "workspaces"), { recursive: true });
    const file = entries.find((entry) => entry.endsWith("native-e2e.txt"));
    expect(file).toBeTruthy();
    expect(await readFile(join(scratch, "workspaces", file!), "utf8")).toBe("after");

    await page.goto(`/${workspace.slug}/agents/new/ai`, { waitUntil: "domcontentloaded" });
    await expect(page.getByText("native-browser-fixture", { exact: true })).toBeVisible({ timeout: 60_000 });
    await page.getByRole("button", { name: "Start conversation", exact: true }).click();
    await expect(page).toHaveURL(/\/agents\/new\/ai\/[^/?]+/, { timeout: 60_000 });
    const builderComposer = page.locator("section").filter({
      has: page.getByRole("heading", { name: "Agent Builder", exact: true }),
    }).locator('[contenteditable="true"]');
    await expect(builderComposer).toBeVisible({ timeout: 60_000 });
    const agentCreates: string[] = [];
    page.on("request", (request) => {
      if (request.url().endsWith("/api/agents") && request.method() === "POST") agentCreates.push(request.postData() ?? "");
    });
    await builderComposer.fill("Design an agent that reviews a bounded change.");
    await builderComposer.press("Control+Enter");
    await expect(page.getByRole("textbox", { name: "Name", exact: true })).toHaveValue("Native builder reviewer", { timeout: 40_000 });
    expect(builderRequests).toBe(1);
    expect(agentCreates).toHaveLength(0);
    await expect(page.getByText("Review this agent before creating it.", { exact: true })).toBeVisible();
    await expect(page.getByText("native-browser-fixture", { exact: true })).toBeVisible();
    const builderCreated = page.waitForResponse((result) => result.url().endsWith("/api/agents") && result.request().method() === "POST");
    await page.getByRole("button", { name: "Create & open agent", exact: true }).click();
    const builderResponse = await builderCreated;
    expect(builderResponse.ok()).toBe(true);
    const builtAgent = await builderResponse.json();
    expect(builtAgent.name).toBe("Native builder reviewer");
    expect(builtAgent.model).toBe("");
    expect(builtAgent.runtime_id).toBe(agent.runtime_id);
    expect(builtAgent.instructions).toContain("Ask before changing files.");
    expect(agentCreates).toHaveLength(1);
    await expect(page).toHaveURL(new RegExp(`/agents/${builtAgent.id}`), { timeout: 60_000 });

    await page.goto(`/${workspace.slug}/chat?session=${mikaSessionId}`, { waitUntil: "domcontentloaded" });
    const mikaComposer = page.locator('[contenteditable="true"]').last();
    await expect(mikaComposer).toBeVisible({ timeout: 60_000 });
    await mikaComposer.fill("Preview native onboarding goal: create a project and one acceptance report issue assigned to you.");
    await mikaComposer.press("Control+Enter");
    await expect(page.getByTestId("virtuoso-item-list").getByText("Preview: create the Native onboarding project and one report issue assigned to Mika. Start it after confirmation?", { exact: true })).toBeVisible({ timeout: 40_000 });
    expect(mikaLoadedSkill).toBe(true);
    expect(mikaKickoffSeen).toBe(true);
    expect(mikaWrites).toBe(0);
    await mikaComposer.fill("Confirm native onboarding plan. Create the project and start the report issue now.");
    await mikaComposer.press("Control+Enter");
    await expect.poll(() => mikaIssueId, { timeout: 40_000 }).not.toBe("");
    await expect(page.getByTestId("virtuoso-item-list").getByText(/Progress and results live on the issue/)).toBeVisible({ timeout: 40_000 });
    await expect.poll(async () => {
      const runs = await api.listIssueRuns(mikaIssueId);
      if (runs.some((run) => run.status === "failed")) throw new Error(`Mika issue failed; fixture errors: ${modelErrors.join(", ")}`);
      return runs.some((run) => run.agent_id === mikaAgentId && run.status === "completed");
    }, { timeout: 40_000 }).toBe(true);
    const onboardingIssue = await api.getIssue(mikaIssueId);
    expect(onboardingIssue.status).toBe("in_review");
    expect(onboardingIssue.assignee_id).toBe(mikaAgentId);
    expect(onboardingIssue.project_id).toBe(mikaProjectId);
    expect(mikaWrites).toBe(2);

    await page.getByRole("button", { name: "Let the daily digest write itself", exact: true }).click();
    await expect(page.getByTestId("virtuoso-item-list").getByText("Preview: daily workspace digest at 09:00 Asia/Shanghai, assigned to Mika. Confirm to schedule it.", { exact: true })).toBeVisible({ timeout: 40_000 });
    expect(digestWrites).toBe(0);
    const exited = once(daemon, "exit");
    daemon.kill("SIGTERM");
    await exited;
    daemon = launchDaemon();
    await expect.poll(async () => (await api.listRuntimes()).some((runtime) => runtime.id === agent.runtime_id && runtime.status === "online"), { timeout: 30_000 }).toBe(true);
    await mikaComposer.fill("Confirm native digest schedule at 09:00 Asia/Shanghai.");
    await mikaComposer.press("Control+Enter");
    await expect(page.getByTestId("virtuoso-item-list").getByText("Scheduled your daily digest for 09:00 Asia/Shanghai. You can pause it in Automations.", { exact: true })).toBeVisible({ timeout: 40_000 });
    expect(digestRetried).toBe(true);
    expect(digestHistoryRecovered).toBe(true);
    expect(digestWrites).toBe(2);
    expect(mikaWrites).toBe(2);
    const digest = await api.getAutopilot(digestId);
    expect(digest.autopilot.assignee_id).toBe(mikaAgentId);
    expect(digest.autopilot.execution_mode).toBe("run_only");
    expect(digest.triggers).toHaveLength(1);
    expect(digest.triggers[0]).toMatchObject({ kind: "schedule", cron_expression: "0 9 * * *", timezone: "Asia/Shanghai" });

    const source = await api.createIssue("Native captured source", { status: "backlog", description: "Native captured historical facts" });
    const anchor = await api.createComment(source.id, "Native source branch point");
    const original = await api.uploadIssueFile(source.id, new Blob([Uint8Array.from(captureImage)], { type: "image/png" }), "native-capture.png");
    await api.updateIssue(source.id, { description: `Native captured historical facts\n\n![native-capture.png](/api/attachments/${original.id}/download)` });
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto(`/${workspace.slug}/issues/${source.id}`, { waitUntil: "domcontentloaded" });
    await page.locator(`#comment-${anchor.id}`).getByText("Native source branch point", { exact: true }).hover({ timeout: 60_000 });
    await page.locator(`#comment-${anchor.id}`).getByRole("button", { name: "Comment actions" }).click({ timeout: 60_000 });
    await page.getByRole("menuitem", { name: "Create sub-issue from here", exact: true }).click();
    const captureDialog = page.getByRole("dialog", { name: "Quick create issue", exact: true });
    await captureDialog.getByRole("button", { name: /Context from .*issue description/ }).click();
    await expect(captureDialog.getByText("Native captured historical facts", { exact: true })).toBeVisible();
    await expect(captureDialog.getByRole("img", { name: "native-capture.png", exact: true })).toBeVisible();
    await captureDialog.getByRole("button", { name: /Created by/ }).click();
    await page.getByRole("button", { name: /^Native browser agent(?: Native browser agent)?$/ }).click();
    await captureDialog.locator('[contenteditable="true"]').fill("Native capture request: create one follow-up using this context.");
    const captured = page.waitForResponse((response) => response.url().endsWith(`/api/comments/${anchor.id}/sub-issues`) && response.request().method() === "POST");
    await captureDialog.getByRole("button", { name: "Create", exact: true }).click();
    expect((await captured).status()).toBe(202);
    await expect.poll(() => capturedIssueId, { timeout: 40_000 }).not.toBe("");
    const capturedIssue = await api.getIssue(capturedIssueId);
    expect(capturedIssue.parent_issue_id).toBe(source.id);
    const cloned = capturedIssue.source_context?.snapshot.source_issue.attachments;
    expect(cloned).toHaveLength(1);
    expect(cloned?.[0].id).not.toBe(original.id);
    expect(cloned?.[0].source_attachment_id).toBe(original.id);
    expect(capturedWrites).toBe(1);
    expect(capturedImageSeen).toBe(true);
    await api.updateIssue(source.id, { description: "Changed after native capture" });
    await page.goto(`/${workspace.slug}/issues/${capturedIssueId}`, { waitUntil: "domcontentloaded" });
    await page.getByRole("button", { name: "Context snapshot", exact: true }).click({ timeout: 60_000 });
    await expect(page.getByRole("dialog").getByText("Native captured historical facts", { exact: true })).toBeVisible();
    expect(modelErrors).toEqual([]);
    expect(modelRequests.every((request) => request.model === "native-browser-fixture")).toBe(true);
  } finally {
    if (testInfo.status !== testInfo.expectedStatus) {
      await testInfo.attach("model-fixture-errors", { body: JSON.stringify(modelErrors), contentType: "application/json" });
      const log = await readFile(join(profileDir, "daemon.log"), "utf8").catch(() => "Daemon log unavailable");
      await testInfo.attach("native-daemon-log", { body: log, contentType: "text/plain" });
    }
    if (daemon && daemon.exitCode === null) {
      const exited = once(daemon, "exit");
      daemon.kill("SIGTERM");
      await exited;
    }
    if (workspaceCreated) await api.deleteWorkspace();
    upstream.close();
    await rm(profileDir, { recursive: true, force: true });
    await rm(scratch, { recursive: true, force: true });
  }
});
