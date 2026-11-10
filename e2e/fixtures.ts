/**
 * TestApiClient — lightweight API helper for E2E test data setup/teardown.
 *
 * Uses raw fetch so E2E tests have zero build-time coupling to the web app.
 */

import "./env";
import pg from "pg";

// `||` (not `??`) so an empty `NEXT_PUBLIC_API_URL=` in .env still falls
// back to localhost. dotenv sets unset-vs-empty both as "" — treating them
// the same matches user intent.
const API_BASE = process.env.NEXT_PUBLIC_API_URL || `http://localhost:${process.env.PORT || "8080"}`;
const DATABASE_URL = process.env.DATABASE_URL ?? "postgres://multica:multica@localhost:5432/multica?sslmode=disable";

interface TestWorkspace {
  id: string;
  name: string;
  slug: string;
}

export type TestIssueStatus =
  | "backlog"
  | "todo"
  | "in_progress"
  | "in_review"
  | "done"
  | "blocked"
  | "cancelled";

export type TestIssuePriority = "urgent" | "high" | "medium" | "low" | "none";

export interface TestTableIssueSeed {
  title: string;
  status?: TestIssueStatus;
  priority?: TestIssuePriority;
  parentIssueId?: string | null;
  position?: number;
}

export interface TestTableIssue {
  id: string;
  title: string;
  status: TestIssueStatus;
  number: number;
}

export class TestApiClient {
  private token: string | null = null;
  private workspaceSlug: string | null = null;
  private workspaceId: string | null = null;
  private email: string | null = null;
  private createdIssueIds: string[] = [];
  private createdProjectIds: string[] = [];
  private seededIssueIds: string[] = [];

  async login(email: string, name: string) {
    const client = new pg.Client(DATABASE_URL);
    await client.connect();
    try {
      // Explicit test-only provisioning preserves test identities across reruns.
      const username = `e2e_${(await client.query("SELECT md5($1::text) AS hash", [email])).rows[0].hash.slice(0, 24)}`;
      const password = "Multica-e2e-password-2026";
      await client.query(`INSERT INTO "user" (name, email) VALUES ($1, $2)
        ON CONFLICT (email) WHERE email <> '' DO NOTHING`, [name, email]);
      await client.query(`UPDATE "user" SET username = $1, password_hash = crypt($2, gen_salt('bf', 12)) WHERE email = $3`, [username, password, email]);
      const response = await fetch(`${API_BASE}/auth/login`, {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username, password }),
      });
      if (!response.ok) throw new Error(`password login failed: ${response.status}`);
      const data = await response.json();
      this.token = data.token;
      this.email = email;
      if (name && data.user?.name !== name) await this.authedFetch("/api/me", { method: "PATCH", body: JSON.stringify({ name }) });
      return data;
    } finally { await client.end(); }
  }

  async loginWithPassword(username: string, password: string) {
    const res = await fetch(`${API_BASE}/auth/login`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ username, password }) });
    if (!res.ok) throw new Error(`Password login failed: ${res.status}`);
    const data = await res.json();
    this.token = data.token;
    return data;
  }

  async createWorkspace(name: string, slug: string): Promise<TestWorkspace> {
    const res = await this.authedFetch("/api/workspaces", { method: "POST", body: JSON.stringify({ name, slug }) });
    if (!res.ok) throw new Error(`Create workspace failed: ${res.status}`);
    const workspace: TestWorkspace = await res.json();
    this.workspaceId = workspace.id;
    this.workspaceSlug = workspace.slug;
    return workspace;
  }

  async registerModelTestRuntime() {
    const res = await this.authedFetch("/api/daemon/register", {
      method: "POST", headers: { "X-Client-Capabilities": "native-runtime-v1" },
      body: JSON.stringify({ workspace_id: this.workspaceId, daemon_id: `models-e2e-${Date.now()}`, device_name: "Models UI fixture", runtimes: [{ name: "Models UI fixture", type: "native", version: "fixture", status: "online" }] }),
    });
    if (!res.ok) throw new Error(`Register model fixture runtime failed: ${res.status}`);
    return res.json();
  }

  async listWorkspaceModels(): Promise<Array<{ id: string; name: string; has_api_key: boolean; is_default: boolean; model: string }>> {
    const res = await this.authedFetch(`/api/workspaces/${this.workspaceId}/models`);
    if (!res.ok) throw new Error(`List models failed: ${res.status}`);
    return res.json();
  }

  async getWorkspaces(): Promise<TestWorkspace[]> {
    const res = await this.authedFetch("/api/workspaces");
    return res.json();
  }

  setWorkspaceId(id: string) {
    this.workspaceId = id;
  }

  setWorkspaceSlug(slug: string) {
    this.workspaceSlug = slug;
  }

  async ensureWorkspace(name = "E2E Workspace", slug = "e2e-workspace") {
    const workspaces = await this.getWorkspaces();
    const workspace = workspaces.find((item) => item.slug === slug) ?? workspaces[0];
    if (workspace) {
      this.workspaceId = workspace.id;
      this.workspaceSlug = workspace.slug;
      return workspace;
    }

    const res = await this.authedFetch("/api/workspaces", {
      method: "POST",
      body: JSON.stringify({ name, slug }),
    });
    if (res.ok) {
      const created = (await res.json()) as TestWorkspace;
      this.workspaceId = created.id;
      this.workspaceSlug = created.slug;
      return created;
    }

    const refreshed = await this.getWorkspaces();
    const created = refreshed.find((item) => item.slug === slug) ?? refreshed[0];
    if (created) {
      this.workspaceId = created.id;
      this.workspaceSlug = created.slug;
      return created;
    }

    throw new Error(`Failed to ensure workspace ${slug}: ${res.status} ${res.statusText}`);
  }

  async markUserOnboarded() {
    if (!this.email) {
      throw new Error("Cannot mark E2E user onboarded before login");
    }

    const client = new pg.Client(DATABASE_URL);
    await client.connect();
    try {
      const result = await client.query(
        `
          UPDATE "user"
          SET
            onboarded_at = COALESCE(onboarded_at, now()),
            onboarding_questionnaire = COALESCE(onboarding_questionnaire, '{}'::jsonb)
              || '{"source":["friends_colleagues"],"source_other":null,"source_skipped":false}'::jsonb
          WHERE email = $1
        `,
        [this.email],
      );
      if (result.rowCount !== 1) {
        throw new Error(`Failed to mark E2E user onboarded: ${this.email}`);
      }
    } finally {
      await client.end();
    }
  }

  async listRuntimes(): Promise<Array<{ id: string; daemon_id: string; provider: string; status: string }>> {
    const res = await this.authedFetch("/api/runtimes");
    if (!res.ok) throw new Error(`list runtimes failed: ${res.status}`);
    return res.json();
  }

  async createChatSession(agentId: string): Promise<{ id: string }> {
    const res = await this.authedFetch("/api/chat/sessions", {
      method: "POST",
      body: JSON.stringify({ agent_id: agentId, title: "Native acceptance" }),
    });
    if (!res.ok) throw new Error(`create chat session failed: ${res.status} ${await res.text()}`);
    return res.json();
  }

  async getAutopilot(id: string): Promise<{
    autopilot: { assignee_id: string; execution_mode: string };
    triggers: Array<{ kind: string; cron_expression: string; timezone: string }>;
  }> {
    const res = await this.authedFetch(`/api/autopilots/${id}`);
    if (!res.ok) throw new Error(`read automation failed: ${res.status}`);
    return res.json();
  }

  async getIssue(id: string): Promise<{
    id: string; status: string; assignee_id: string; project_id: string; parent_issue_id?: string;
    source_context?: { snapshot: { source_issue: { attachments: Array<{ id: string; source_attachment_id?: string }> } } };
  }> {
    const res = await this.authedFetch(`/api/issues/${id}`);
    if (!res.ok) throw new Error(`read issue failed: ${res.status}`);
    return res.json();
  }

  async listIssueRuns(id: string): Promise<Array<{ id: string; agent_id: string; status: string }>> {
    const res = await this.authedFetch(`/api/issues/${id}/task-runs`);
    if (!res.ok) throw new Error(`list issue runs failed: ${res.status}`);
    return res.json();
  }

  /** Create a project and register it for cleanup. */
  async createProject(title: string, opts?: Record<string, unknown>) {
    const res = await this.authedFetch("/api/projects", {
      method: "POST",
      body: JSON.stringify({ title, ...opts }),
    });
    if (!res.ok) {
      throw new Error(`create project failed: ${res.status} ${await res.text()}`);
    }
    const project = await res.json();
    this.createdProjectIds.push(project.id);
    return project as { id: string; title: string; status: string };
  }

  async updateProject(id: string, updates: Record<string, unknown>) {
    const res = await this.authedFetch(`/api/projects/${id}`, {
      method: "PUT",
      body: JSON.stringify(updates),
    });
    if (!res.ok) {
      throw new Error(`update project failed: ${res.status} ${await res.text()}`);
    }
    return res.json();
  }

  async deleteProject(id: string) {
    await this.authedFetch(`/api/projects/${id}`, { method: "DELETE" });
  }

  async createIssue(title: string, opts?: Record<string, unknown>) {
    const res = await this.authedFetch("/api/issues", {
      method: "POST",
      body: JSON.stringify({ title, ...opts }),
    });
    const issue = await res.json();
    this.createdIssueIds.push(issue.id);
    return issue;
  }

  /**
   * Insert a large, deterministic issue fixture in one transaction.
   *
   * Browser E2E coverage for cursor-backed Table views needs 1,000+ rows,
   * which would make setup itself dominate the test if every row went through
   * the HTTP create endpoint. These rows intentionally contain no dependent
   * records; cleanup deletes exactly the returned IDs from the isolated E2E
   * workspace.
   */
  async seedTableIssues(rows: TestTableIssueSeed[]): Promise<TestTableIssue[]> {
    if (rows.length === 0) return [];
    if (!this.workspaceId || !this.email) {
      throw new Error("Cannot seed table issues before login and workspace setup");
    }

    const client = new pg.Client(DATABASE_URL);
    await client.connect();
    try {
      await client.query("BEGIN");
      const userResult = await client.query<{ id: string }>(
        `SELECT id FROM "user" WHERE email = $1`,
        [this.email],
      );
      const creatorId = userResult.rows[0]?.id;
      if (!creatorId) {
        throw new Error(`Cannot resolve E2E creator for ${this.email}`);
      }

      const counterResult = await client.query<{ issue_counter: number }>(
        `
          UPDATE workspace
          SET issue_counter = issue_counter + $2
          WHERE id = $1
          RETURNING issue_counter
        `,
        [this.workspaceId, rows.length],
      );
      const finalCounter = Number(counterResult.rows[0]?.issue_counter);
      if (!Number.isFinite(finalCounter)) {
        throw new Error(`Cannot reserve issue numbers for workspace ${this.workspaceId}`);
      }
      const firstNumber = finalCounter - rows.length + 1;

      const inserted = await client.query<TestTableIssue>(
        `
          INSERT INTO issue (
            workspace_id,
            title,
            status,
            priority,
            creator_type,
            creator_id,
            parent_issue_id,
            position,
            number
          )
          SELECT
            $1::uuid,
            fixture.title,
            fixture.status,
            fixture.priority,
            'member',
            $2::uuid,
            fixture.parent_issue_id,
            fixture.position,
            fixture.number
          FROM unnest(
            $3::text[],
            $4::text[],
            $5::text[],
            $6::uuid[],
            $7::double precision[],
            $8::integer[]
          ) WITH ORDINALITY AS fixture(
            title,
            status,
            priority,
            parent_issue_id,
            position,
            number,
            ordinal
          )
          ORDER BY fixture.ordinal
          RETURNING id, title, status, number
        `,
        [
          this.workspaceId,
          creatorId,
          rows.map((row) => row.title),
          rows.map((row) => row.status ?? "backlog"),
          rows.map((row) => row.priority ?? "none"),
          rows.map((row) => row.parentIssueId ?? null),
          rows.map((row, index) => row.position ?? index + 1),
          rows.map((_row, index) => firstNumber + index),
        ],
      );
      await client.query("COMMIT");
      this.seededIssueIds.push(...inserted.rows.map((row) => row.id));
      return inserted.rows;
    } catch (error) {
      await client.query("ROLLBACK");
      throw error;
    } finally {
      await client.end();
    }
  }

  async deleteIssue(id: string) {
    await this.authedFetch(`/api/issues/${id}`, { method: "DELETE" });
  }

  async createComment(issueId: string, content: string) {
    const res = await this.authedFetch(`/api/issues/${issueId}/comments`, {
      method: "POST",
      body: JSON.stringify({ content }),
    });
    if (!res.ok) {
      throw new Error(`create comment failed: ${res.status} ${await res.text()}`);
    }
    return (await res.json()) as { id: string };
  }

  async uploadIssueFile(issueId: string, file: Blob, filename: string): Promise<{ id: string }> {
    const body = new FormData();
    body.set("issue_id", issueId);
    body.set("file", file, filename);
    const res = await this.authedFetch("/api/upload-file", { method: "POST", body });
    if (!res.ok) throw new Error(`upload issue file failed: ${res.status}`);
    return res.json();
  }

  /** Adds an existing user to this client's workspace and returns the member id. */
  async addMemberByEmail(email: string, role: "admin" | "member" = "member"): Promise<string> {
    if (!this.workspaceId) throw new Error("Cannot add a member before a workspace is selected");
    const client = new pg.Client(DATABASE_URL);
    await client.connect();
    try {
      const result = await client.query(
        `INSERT INTO member (workspace_id, user_id, role)
         SELECT $1, id, $3 FROM "user" WHERE email = $2
         RETURNING id`,
        [this.workspaceId, email, role],
      );
      if (result.rowCount !== 1) throw new Error(`No user with email ${email}`);
      return result.rows[0].id as string;
    } finally {
      await client.end();
    }
  }

  async removeMember(memberId: string) {
    const res = await this.authedFetch(`/api/workspaces/${this.workspaceId}/members/${memberId}`, {
      method: "DELETE",
    });
    if (!res.ok) {
      throw new Error(`remove member failed: ${res.status} ${await res.text()}`);
    }
  }

  /** Deletes this client's workspace; only for workspaces a test created. */
  async deleteWorkspace() {
    if (!this.workspaceId) return;
    await this.authedFetch(`/api/workspaces/${this.workspaceId}`, { method: "DELETE" });
  }

  /** Server-side issue search, for comparing against what the UI shows. */
  async searchIssues(q: string, opts: { includeClosed?: boolean; limit?: number } = {}) {
    const params = new URLSearchParams({ q, limit: String(opts.limit ?? 20) });
    if (opts.includeClosed) params.set("include_closed", "true");
    const res = await this.authedFetch(`/api/issues/search?${params}`);
    if (!res.ok) {
      throw new Error(`search issues failed: ${res.status} ${await res.text()}`);
    }
    return (await res.json()) as { issues: { id: string; identifier: string; title: string }[] };
  }

  async updateIssue(id: string, updates: Record<string, unknown>) {
    const res = await this.authedFetch(`/api/issues/${id}`, {
      method: "PUT",
      body: JSON.stringify(updates),
    });
    if (!res.ok) {
      throw new Error(`update issue failed: ${res.status} ${await res.text()}`);
    }
    return res.json();
  }

  /** Clean up all issues created during this test. */
  async cleanup() {
    if (this.seededIssueIds.length > 0 && this.workspaceId) {
      const client = new pg.Client(DATABASE_URL);
      await client.connect();
      try {
        await client.query(
          `DELETE FROM issue WHERE workspace_id = $1 AND id = ANY($2::uuid[])`,
          [this.workspaceId, this.seededIssueIds],
        );
      } finally {
        await client.end();
      }
      this.seededIssueIds = [];
    }
    for (const id of this.createdIssueIds) {
      try {
        await this.deleteIssue(id);
      } catch {
        /* ignore — may already be deleted */
      }
    }
    this.createdIssueIds = [];
    // Projects last: an issue delete leaves no project reference behind, and
    // dropping the project first would strand the issues in the list.
    for (const id of this.createdProjectIds) {
      try {
        await this.deleteProject(id);
      } catch {
        /* ignore — may already be deleted */
      }
    }
    this.createdProjectIds = [];
  }

  getToken() {
    return this.token;
  }

  /** Remove only fresh password-auth test identities that never joined a workspace. */
  async removePasswordTestAccount(username: string) {
    if (!username.startsWith("e2e_password_")) throw new Error("Expected password-auth test username");
    const client = new pg.Client(DATABASE_URL);
    await client.connect();
    try {
      await client.query(`DELETE FROM "user" u WHERE username = $1
        AND NOT EXISTS (SELECT 1 FROM member m WHERE m.user_id = u.id)`, [username]);
    } finally {
      await client.end();
    }
  }

  getEmail() {
    if (!this.email) {
      throw new Error("Test API client is not logged in");
    }
    return this.email;
  }

  private async authedFetch(path: string, init?: RequestInit) {
    const headers: Record<string, string> = {
      ...(init?.body instanceof FormData ? {} : { "Content-Type": "application/json" }),
      ...((init?.headers as Record<string, string>) ?? {}),
    };
    if (this.token) headers["Authorization"] = `Bearer ${this.token}`;
    if (this.workspaceSlug) headers["X-Workspace-Slug"] = this.workspaceSlug;
    else if (this.workspaceId) headers["X-Workspace-ID"] = this.workspaceId;
    return fetch(`${API_BASE}${path}`, { ...init, headers });
  }
}
