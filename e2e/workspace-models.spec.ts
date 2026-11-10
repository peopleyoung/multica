import { test, expect } from "@playwright/test";
import { spawn, type ChildProcess } from "node:child_process";
import { once } from "node:events";
import { createServer, request as httpRequest } from "node:http";
import { request as httpsRequest } from "node:https";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { homedir, tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { TestApiClient } from "./fixtures";

// Explicit credentials select an existing test/operator account. All feature
// data lives in a fresh workspace and is removed through TestApiClient.
test("workspace models and native execution settings apply per agent", async ({ page }, testInfo) => {
  const username = process.env.MULTICA_MODELS_E2E_USERNAME;
  const password = process.env.MULTICA_MODELS_E2E_PASSWORD;
  test.skip(!username || !password, "Requires explicit password credentials");
  test.setTimeout(180_000);
  const api = new TestApiClient();
  const slug = `e2e-models-${Date.now().toString(36)}`;
  await api.loginWithPassword(username!, password!);
  const workspace = await api.createWorkspace("Models acceptance", slug);
  let daemon: ChildProcess | undefined;
  let fixtureRuntimeID = "";
  let profileDir = "";
  let scratch = "";
  let endpoint = "http://127.0.0.1:1/v1";
  const streamModels: string[] = [];
  const errors: string[] = [];
  // User tokens discover all memberships. Limit the fixture daemon to its
  // workspace so acceptance never registers or claims work in another one.
  const daemonAPI = createServer((req, res) => {
    if (req.url === "/api/daemon/workspaces") {
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify([{ id: workspace.id, name: workspace.name }]));
      return;
    }
    const target = new URL(req.url!, process.env.NEXT_PUBLIC_API_URL);
    const forward = target.protocol === "https:" ? httpsRequest : httpRequest;
    const forwarded = forward(target, {
      method: req.method,
      headers: { ...req.headers, host: target.host },
    }, response => {
      res.writeHead(response.statusCode!, response.headers);
      response.pipe(res);
    });
    forwarded.on("error", () => { res.writeHead(502).end(); });
    req.pipe(forwarded);
  });
  daemonAPI.on("upgrade", (_req, socket) => socket.destroy());
  const upstream = createServer(async (req, res) => {
    try {
      let raw = "";
      for await (const chunk of req) raw += chunk;
      const body = JSON.parse(raw);
      if (!["alpha", "beta"].includes(body.model) || req.headers.authorization !== `Bearer fixture-${body.model}-key`) throw new Error("Unexpected model or credential");
      if (body.stream) {
        streamModels.push(body.model);
        const tools = new Set((body.tools ?? []).map((tool: { function?: { name?: string } }) => tool.function?.name));
        const enabled = body.model === "alpha";
        for (const name of ["make_directory", "apply_patch", "shell", "job_output", "job_list", "job_kill", "http_request"]) {
          if (tools.has(name) !== enabled) throw new Error("Native capability configuration did not match the agent");
        }
        if (!tools.has("native_runtime_info")) throw new Error("Runtime diagnostics are missing");
        const results = body.messages.filter((message: { role: string }) => message.role === "tool");
        let toolCall: { name: string; arguments: string } | undefined;
        if (enabled && results.length === 0) {
          toolCall = { name: "shell", arguments: JSON.stringify({ command: "sleep 0.2; printf native-job-collected", run_in_background: true }) };
        } else if (enabled && results.length === 1) {
          const admission = JSON.parse(results[0].content);
          if (!admission.job_id) throw new Error("Missing native job admission handle");
          toolCall = { name: "job_output", arguments: JSON.stringify({ job_id: admission.job_id, wait_ms: 2000 }) };
        } else if (enabled) {
          const admission = JSON.parse(results[0].content);
          const settled = JSON.parse(results[1].content);
          if (settled.job_id !== admission.job_id || settled.status !== "completed" || settled.output !== "native-job-collected") {
            throw new Error("Native job was not collected through its original handle");
          }
        }
        res.setHeader("Content-Type", "text/event-stream");
        if (toolCall) {
          res.end(`data: ${JSON.stringify({ choices: [{ index: 0, delta: { tool_calls: [{ index: 0, id: `native-job-${results.length}`, type: "function", function: toolCall }] }, finish_reason: "tool_calls" }] })}\n\ndata: [DONE]\n\n`);
          return;
        }
        res.end(`data: ${JSON.stringify({ choices: [{ index: 0, delta: { content: `Reply from ${body.model}.` }, finish_reason: "stop" }] })}\n\ndata: ${JSON.stringify({ choices: [], usage: { prompt_tokens: 100, completion_tokens: 10 } })}\n\ndata: [DONE]\n\n`);
      } else {
        res.setHeader("Content-Type", "application/json");
        res.end(JSON.stringify({ choices: [{ index: 0, message: { role: "assistant", content: "Model routing acceptance" }, finish_reason: "stop" }], usage: { prompt_tokens: 100, completion_tokens: 10 } }));
      }
    } catch (error) { errors.push(String(error)); res.writeHead(500).end(); }
  });
  try {
    if (process.env.MULTICA_NATIVE_E2E_BINARY) {
      upstream.listen(0, "0.0.0.0");
      await once(upstream, "listening");
      const address = upstream.address();
      if (!address || typeof address === "string") throw new Error("Missing fixture address");
      endpoint = `http://${new URL(process.env.NEXT_PUBLIC_API_URL!).hostname}:${address.port}/v1`;
      scratch = await mkdtemp(join(tmpdir(), "multica-models-"));
      profileDir = join(homedir(), ".multica", "profiles", slug);
      await mkdir(profileDir, { recursive: true, mode: 0o700 });
      daemonAPI.listen(0, "127.0.0.1");
      await once(daemonAPI, "listening");
      const proxyAddress = daemonAPI.address();
      if (!proxyAddress || typeof proxyAddress === "string") throw new Error("Missing daemon fixture address");
      const daemonURL = `http://127.0.0.1:${proxyAddress.port}`;
      await writeFile(join(profileDir, "config.json"), JSON.stringify({ token: api.getToken(), workspace_id: workspace.id, server_url: daemonURL }), { mode: 0o600 });
      const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("MULTICA_") && key !== "OPENAI_API_KEY"));
      const commands = (await readFile(resolve("scripts/agent-cli-command-names.txt"), "utf8")).split("\n").filter(name => name && !name.startsWith("#"));
      for (const name of commands) env[`MULTICA_${name.toUpperCase().replaceAll("-", "_")}_PATH`] = join(scratch, "missing", name);
      env.MULTICA_GC_ENABLED = "false";
      env.MULTICA_DAEMON_ID = slug;
      env.MULTICA_NATIVE_ALLOW_WRITE = "true";
      env.MULTICA_NATIVE_ALLOW_SHELL = "true";
      env.MULTICA_NATIVE_HTTP_ORIGINS = new URL(endpoint).origin;
      daemon = spawn(process.env.MULTICA_NATIVE_E2E_BINARY, ["daemon", "start", "--foreground", "--profile", slug, "--server-url", daemonURL, "--workspaces-root", join(scratch, "workspaces"), "--no-auto-update", "--no-auto-reload", "--poll-interval", "1s", "--heartbeat-interval", "2s"], { cwd: scratch, env, stdio: "ignore" });
      await expect.poll(async () => {
        if (daemon!.exitCode !== null) throw new Error("Fixture daemon exited before registration");
        const runtime = (await api.listRuntimes()).find(runtime => runtime.daemon_id === slug && runtime.provider === "native" && runtime.status === "online");
        fixtureRuntimeID = runtime?.id ?? "";
        return !!runtime;
      }, { timeout: 30_000 }).toBe(true);
      await page.route("**/api/runtimes*", async route => {
        if (new URL(route.request().url()).pathname !== "/api/runtimes") return route.continue();
        const response = await route.fetch();
        const runtimes: Array<{ id: string }> = await response.json();
        await route.fulfill({ response, json: runtimes.filter(runtime => runtime.id === fixtureRuntimeID) });
      });
    } else {
      await api.registerModelTestRuntime();
    }
    await page.goto("/login");
    await page.locator("#login-username").fill(username!);
    await page.locator("#login-password").fill(password!);
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
    await page.waitForURL(url => !url.pathname.startsWith("/login"));
    await page.goto(`/${workspace.slug}/settings?tab=models`);
    for (const [name, upstream, key] of [["Model Alpha", "alpha", "fixture-alpha-key"], ["Model Beta", "beta", "fixture-beta-key"]]) {
      await page.getByRole("button", { name: "Add model", exact: true }).click();
      await page.getByLabel("Display name", { exact: true }).fill(name);
      await page.getByLabel("OpenAI-compatible API base URL", { exact: true }).fill(endpoint);
      await page.getByLabel("Model ID", { exact: true }).fill(upstream);
      await page.getByLabel("API key (optional)", { exact: true }).fill(key);
      await page.getByLabel("Supports streaming tool calls", { exact: true }).check();
      const saved = page.waitForResponse(r => r.url().endsWith(`/workspaces/${workspace.id}/models`) && r.request().method() === "POST");
      await page.getByRole("button", { name: "Save", exact: true }).click();
      const response = await saved;
      expect(response.status()).toBe(201);
      expect(await response.text()).not.toContain(key);
      await expect(page.getByRole("dialog")).toBeHidden();
      await expect(page.getByText(name, { exact: false })).toBeVisible();
    }
    const models = await api.listWorkspaceModels();
    expect(models.map(m => m.name).sort()).toEqual(["Model Alpha", "Model Beta"]);
    expect(models.every(m => m.has_api_key)).toBe(true);
    for (const name of ["Model Alpha", "Model Beta"]) {
      await page.goto(`/${workspace.slug}/agents/new/manual`);
      await page.locator("#agent-create-name").fill(`Agent ${name}`);
      const picker = page.getByRole("combobox", { name: "Model", exact: true });
      await expect(picker).toBeEnabled();
      await picker.click();
      await page.getByRole("option", { name, exact: true }).click();
      const created = page.waitForResponse(r => r.url().endsWith("/api/agents") && r.request().method() === "POST");
      await page.getByRole("button", { name: "Create & open agent", exact: true }).click();
      const response = await created;
      expect(response.status()).toBe(201);
      const agent = await response.json();
      expect(agent.model).toBe(models.find(m => m.name === name)?.id);
      if (daemon) expect(agent.runtime_id).toBe(fixtureRuntimeID);
      await page.waitForURL(url => !url.pathname.endsWith("/new/manual"));
      if (daemon) {
        if (name === "Model Alpha") {
          await page.goto(`/${workspace.slug}/agents/${agent.id}?view=runtime_config`);
          await expect(page.getByRole("heading", { name: "Runtime configuration", exact: true })).toBeVisible();
          await page.getByRole("switch", { name: "Write files in the working directory", exact: true }).check();
          await page.getByRole("switch", { name: "Run scripts and commands", exact: true }).check();
          await page.getByRole("button", { name: "Add service", exact: true }).click();
          await page.getByLabel("Service name", { exact: true }).fill("fixture");
          await page.getByLabel("API base URL", { exact: true }).fill(endpoint);
          const saved = page.waitForResponse(r => r.url().endsWith(`/api/agents/${agent.id}`) && r.request().method() === "PUT");
          await page.getByRole("button", { name: "Save", exact: true }).click();
          expect((await saved).status()).toBe(200);
          await page.reload();
          await expect(page.getByRole("switch", { name: "Run scripts and commands", exact: true })).toBeChecked();
          await expect(page.getByText("Long commands return a job ID", { exact: false })).toBeVisible();
          await expect(page.getByLabel("Service name", { exact: true })).toHaveValue("fixture");
          await page.screenshot({ path: testInfo.outputPath("native-runtime-configured.png"), fullPage: true });
        }
        const session = await api.createChatSession(agent.id);
        await page.goto(`/${workspace.slug}/chat?session=${session.id}`);
        const composer = page.locator('[contenteditable="true"]').last();
        await composer.fill("Verify the selected model.");
        await composer.press("Control+Enter");
        const id = models.find(m => m.name === name)!.model;
        await expect(page.getByTestId("virtuoso-item-list").getByText(`Reply from ${id}.`, { exact: true })).toBeVisible({ timeout: 40_000 });
      }
    }
    await page.goto(`/${workspace.slug}/settings?tab=models`);
    await page.getByRole("button", { name: "Edit Model Alpha", exact: true }).click();
    await expect(page.getByLabel("API key (optional)", { exact: true })).toHaveValue("");
    await page.getByRole("button", { name: "Cancel", exact: true }).click();
    await expect(page.getByRole("dialog")).toBeHidden();
    await page.screenshot({ path: testInfo.outputPath("models-configured.png"), fullPage: true });
    if (daemon) {
      expect(streamModels).toEqual(["alpha", "alpha", "alpha", "beta"]);
      expect(errors).toEqual([]);
    }
  } finally {
    if (daemon && daemon.exitCode === null && daemon.signalCode === null) {
      const stopped = once(daemon, "exit"); daemon.kill("SIGTERM"); await stopped;
    }
    if (upstream.listening) await new Promise<void>(done => upstream.close(() => done()));
    if (daemonAPI.listening) {
      daemonAPI.closeAllConnections();
      await new Promise<void>(done => daemonAPI.close(() => done()));
    }
    if (profileDir) await rm(profileDir, { recursive: true, force: true });
    if (scratch) await rm(scratch, { recursive: true, force: true });
    await api.deleteWorkspace();
    expect((await api.getWorkspaces()).some(w => w.id === workspace.id)).toBe(false);
  }
});
