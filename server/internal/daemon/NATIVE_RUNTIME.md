# Native runtime execution

The API owns model credentials, pinned model selection, session leases and
append-only checkpoints. The daemon owns tool execution. Model requests use only
the projection of committed history; incomplete streamed calls cannot run tools.

## Model wire protocols

Workspace model settings select `capabilities.api`: `chat_completions` (also the
omitted default) or `responses`. The selection is saved with the existing model
capabilities and pinned in each run's model snapshot, with encrypted credentials; editing a model
does not change a running task. Native execution, context summaries and text/JSON
helpers use the selected protocol. A model name never selects a protocol, and a
failed stream never triggers a second request through another protocol.

Responses uses `store=false`, explicit output limits, non-strict function schemas
and provider-neutral function-call/result history. Encrypted reasoning returned
by the endpoint is retained for replay only to the same endpoint, model and
credential. Summaries exclude opaque reasoning but retain the durable original.
A `response.completed` terminal event is required before tool calls can execute;
failed, incomplete, refused or disconnected responses cannot release partial
calls. Both protocols share admission limits and upstream usage accounting.
Provider 400/422, authentication, missing-model and rate-limit failures have
credential-free codes; upstream response bodies never cross the daemon boundary.

## Issue delivery and parent coordination

Native builds its own per-turn prompt instead of loading the CLI runtime brief.
Issue runs, including human-comment retries, receive explicit lifecycle guidance:
read the issue and relevant threads, move work being advanced to `in_progress`,
and move the delivered outcome to `in_review` for acceptance. `done` remains a
human decision. Ongoing work stays in progress, missing prerequisites are reported
as blocked, and ancillary replies leave status alone. Status writes use the
observed issue revision and respect the agent's existing authority.

A child delivery or new blocker requiring coordination is reported once in a
new top-level parent comment with an explicit mention of the parent's actual
assigned squad or agent. The report includes the child, evidence, observed status,
limitations and next step. The child result still follows its requested thread;
plain text in that thread saying the leader should continue does not invoke the
parent's coordinator. Existing invocation permission checks remain in force.

The `child_done` wakeup is a separate closed-stage/all-children barrier. An
`in_review` child is not closed, and completing one unstaged child does not close
that barrier while siblings remain open. The parent report does not accept the
child, its siblings or the parent. A leader verifies the overall outcome before
moving the parent to review; dispatch alone is not delivery.

These are model instructions using the existing business tools, not a universal
completion hook. A successful run does not automatically imply issue delivery,
change issue status or notify the parent. Tests use a deterministic fake model
to check prompt inclusion and the actual authenticated status/comment/invocation
path; they do not prove that every upstream model follows these instructions.

## Managed shell jobs

`tools.Host` owns a job registry for one task execution. `shell` admits a job
before starting its process and returns a structured observation. Its wait limit
is independent of execution: an expired wait returns the same running job.
`job_output` reads status and absolute output offsets, `job_list` discovers this
session's jobs, and `job_kill` cancels a process tree and waits for settlement.
Polling is intentionally excluded from the repeated-unchanged-read heuristic;
the model step limit still bounds a task. Native tasks allow 128 model steps.

The registry has four active slots per session and retains at most 128 admission
records per session. Internal admission identities remain full hashes; model-facing
handles are `shell-` plus 16 hexadecimal characters. Canonical identities from
existing history still identify the same admission. Handle collisions are rejected
before execution. A missing handle tells the model to use `job_list`.

Live output retention is 256 KiB per job; settled disk-backed jobs retain a 16 KiB
tail in memory. Reads are at most 8 KiB. With a journal directory configured,
output is also written to a private log up to 8 MiB per local job. `job_output`
can retrieve the retained prefix by absolute byte offset and `job_search` scans
bounded pages for literal text. Quota loss and disk failures are explicit. A gap
between the disk prefix and memory tail is reported, never presented as complete.
Output is binary-safe in the journal and rendered as valid UTF-8 for the model.
Offsets never consume output, so a lost checkpoint acknowledgement cannot skip
bytes. Truncation reports the earliest available offset. Neither command
environment nor model credentials are copied into the journal; command output
can contain private data, so journal directories/files use 0700/0600 permissions.

Settled jobs produce bounded completion notices before the next model step or
before accepting a final response generated without the completion fact. Notices
are appended to durable history before inference. Terminal observations already
in tool history suppress redundant notices. Unconfirmed delivery does not consume
an output cursor. Delivery is limited to the current execution; it does not create
new platform tasks after the owner has finished.

`read_file` accepts optional absolute byte `offset` and bounded `limit`. Small
unpaged files retain their text response. Larger files and explicit pages return
JSON with `next_offset` and `has_more`; UTF-8 page boundaries and encoded output
limits are enforced. Artifact uploads and patching retain their one MiB limits.

Processes belong to the current task execution. Cancellation and normal host
teardown cancel jobs, await settlement, and clean up descendants. The engine
logs one reminder if a model tries to finish while jobs remain live; a repeated
attempt fails explicitly instead of silently claiming completion. Work that
must outlive a task belongs in a remote durable job runner. Killing SSH never
establishes whether the remote operation stopped.

## Admission and recovery

Session ID and tool-call ID determine an admission's job ID. Canonical argument
digests reject reuse with different arguments. The journal lives outside the
working copy, under the daemon's native result directory, partitioned by hashed
session identity. Atomic replacement and file/directory sync precede publication
on Unix. Windows syncs the file but does not provide the directory-sync guarantee.

The registry is opened under the session's exclusive execution lease. A restored
unsettled record becomes `interrupted`: it is evidence requiring reconciliation,
never an instruction to launch again. No PID-based reattachment or signalling is
attempted after restart because PID reuse would target unrelated processes.

Before ordinary started-to-unknown recovery, the engine can recover a **started
shell call's existing admission receipt**. This records successful admission,
not successful execution; its result still carries `interrupted`, `failed`, or
`cancelled` where applicable. It executes no command. Independent service
submissions can likewise recover a receipt without resubmitting.

Started calls without receipts become unknown. An interrupted tool group is
closed with canonical `TOOL_OUTCOME_UNKNOWN` results, preserving each original
call and the uncertainty in append-only history. Planned remainders receive
`TOOL_NOT_STARTED`; they are not dispatched automatically. Existing confirmed
results remain unchanged. The model can then investigate external state. Guidance
permits retrying only read-only or demonstrably idempotent work and requires
verification or human input before uncertain side effects. This is model guidance,
not a general proof of idempotence for arbitrary commands. Lease/CAS remains in
force; ordinary saves cannot change an unknown record into succeeded/failed or
replace its recovery result with a fabricated outcome.

The container-local `native-reconcile` can still record an independently verified
terminal outcome for a legacy unknown shell call that remains outstanding. The
command defaults to a dry run and requires a workspace manager, the exact
checkpoint version, call identity, verified terminal outcome and an evidence
file. Applying also requires an exclusive private backup. Database predicates
reject active leases or running/dispatched work in the same scope. The result
records operator, time, evidence and `replayed: false`; no command runs and no
task is requeued. Ordinary daemon saves still reject unknown-to-terminal
transitions. The failure reason `native_tool_outcome_unknown` tells users that
verification is required rather than suggesting blind command replay. Once the
group is closed for model investigation, the audit fact remains unknown; later
verification belongs in subsequent logged observations, not rewritten history.

## Independent jobs

`cmd/native-jobs` runs a separate authenticated job service using the same
admission, output and process-tree implementation. A `multica_jobs` named external
service exposes the `persistent_job` tool for submit/list/output/search/lookup/cancel.
Host origin approval and private Bearer credentials are required. Write permission
on this named service explicitly authorizes remote command submission, independently
of the local shell setting. Submission keys come from the Native session/call
identity, not model arguments. Different credentials map to separate namespaces.

Native task cancellation, daemon restart and API restart do not own these jobs.
The service owns its processes and has configurable per-namespace concurrency,
record and log limits. A private directory lock prevents competing service owners.
On service restart, unsettled jobs become interrupted without replay or PID-based
reattachment; retained logs are recovered from disk. Service shutdown cancels
local jobs. This is not a durable GPU scheduler or an automatic takeover of work
after service failure. See [setup and API](../../../docs/engineering/native-job-service.zh.md).

## Context and failure handling

Compaction retains complete tool-call/result groups and full durable history.
Summary instructions explicitly preserve local and remote job IDs, offsets and
unresolved outcomes. A compaction that does not reduce an oversized request stops
immediately instead of repeatedly requesting ineffective summaries. Model
context sizing remains conservative; no unverified tokenizer is assumed.

Provider SSE errors are classified as upstream failures. Partial streamed calls
remain non-executable, and an interrupted provider request never replays earlier
business tools. Provider availability and remote training/data errors remain
separate from successful runtime execution.

## Reference

This implementation follows the execution contracts in the adjacent
`deepseek-harness` checkout:

- `packages/shell/tool-bash`: every command is a job; a foreground wait can yield
  its existing handle without restarting or killing it.
- `packages/jobs/jobs-local`: exact ownership, bounded output, one lifecycle
  controller, and cancellation on owner disposal.
- `packages/compaction/compaction`: complete tool pairs and reconstructable
  model-visible history.

The implementation remains Go and uses Multica's existing process-tree controller,
API leases, checkpoint transition validation and private result journal.

## One execution environment

The agent's optional `environment` selects `local` (default) or `multica_jobs`.
In the latter mode, the named service owns **all** filesystem tools, root and
nested repository rules, shell jobs, artifact reads and interactive terminals.
Daemon-local working-copy paths are not valid file-tool paths in that mode.
Local stdio MCP processes are disabled; explicitly configured HTTP MCP remains
separate. Runtime metadata reports the selected environment and the remote
host's directory and sandbox facts. This follows deepseek-harness's same-world
filesystem/subprocess composition rather than asking the model to wrap commands
in SSH.

The remote service must run a version exposing `/v1/environment`, allow file
writes in its deployment limits when requested, and be approved by the daemon
owner's HTTP origin policy. Remote shell/file writes additionally require the
agent's named service `allow_write`. Read-only configurations cannot elevate
these powers. A token scopes job visibility and terminal ownership, **not** a
private filesystem: use separate service accounts/work directories for mutually
untrusted workloads. Only confined relative regular-file reads can become
artifacts; the daemon cannot substitute a same-named local file.

## Independent job feedback

Persistent submissions register a private durable observation before dispatch.
Explicitly backgrounded remote shell calls do likewise; foreground calls that
remain running register after observing the receipt, and recovered shell receipts
also register. Lost foreground responses register an observation as well. The
independent watcher reads admission receipts only. It cannot relaunch a command. An uncertain foreground submission needs receipt recovery;
an absent receipt never becomes an instruction to submit with another identity.

After a terminal observation and after the original issue run has ended, the
watcher sends an authenticated daemon event using the existing control-plane
credential. A scoped daemon credential must match the source executor; a
long-lived PAT/JWT must belong to that runtime owner, not merely a workspace
member or admin. A deterministic system comment and at most one follow-up task commit in one transaction. Lost acknowledgements and
daemon restarts reuse the same event identity. The handler stabilizes the agent,
issue and source task, defers while another turn or retry is active, and checks
assignment, closure, cancellation, human workspace membership and invocation
permission. A changed runtime, removed owner
or revoked invocation access withholds automatic follow-up but retains a visible
observation. The follow-up inherits human/accountable provenance and refreshes
connected-app permissions. It inspects existing output/artifacts; neither a
completed process nor a queued follow-up changes issue acceptance or dispatches
a parent coordinator implicitly. Ordinary child delivery instructions still own
that handoff.

Observation files live under `.native-job-watches` (0700 directory, 0600 files,
maximum 4096) and contain a service credential. They are not model history.
Polling is bounded and warnings are aggregated every five minutes. Missing
receipts, revoked origins, authentication failures and lost acknowledgements keep
the obligation rather than silently dropping it. Operators must reconcile these
private records; service-token rotation currently requires updating outstanding
records securely. There is no automatic credential migration or operator UI for
this backlog. Chat-only runs do not create automatic issue follow-ups.

## Context and terminals

For unchanged request prefixes, subsequent model steps use provider-reported
input tokens plus conservative byte reservations for newly appended data. Changed
tools/projections, missing usage and daemon restarts invalidate measurements.
Compaction starts at 80% of the remaining input budget. Without a tokenizer or a
valid provider measurement, estimation remains conservative. Image attachments
retain their separate conservative budget. The model's context limit and the
API's bounded request size still apply.

Compaction preserves recent complete tool groups (up to six additional groups
within a small retained byte budget) and the full durable audit history. A bounded
projection rebuilds job IDs, status and log offsets directly from recognized tool
results, so even an incomplete model summary cannot erase those handles. These
are historical observations, not current status or permission to relaunch.

The Linux `terminal` tool exposes open/send/read/list/resize/interrupt/close over
an actual PTY. Shell/REPL state survives calls; SIGINT targets the foreground
process group. Silence never proves readiness or successful completion. Reads
are bounded to 8 KiB with absolute offsets and a 256 KiB scrollback. Four handles
per engine session and 64 per host, ten-minute idle expiry, bounded input waits
and teardown cleanup limit abandoned terminals. Namespace plus engine-session
identity prevents another session from reading/sending to a terminal. Terminals
are ephemeral and cannot recover after service restart; full-screen programs and
cross-platform terminal support are outside the contract.

An independent service may configure immutable `sandbox` deployment limits:
`unconfined` (default), `workspace_write`, or `read_only`. Confined modes require a
successful Linux bubblewrap functional probe and never fall back to an unconfined
command. One-shot and interactive commands use the same mounts, namespace,
network and credential policy. Read-only mode rejects file-write permissions.
Additional read/device mounts and network access are host-owner choices, not tool
arguments. This does not provide CPU/RAM/GPU quotas, per-agent filesystem tenants,
a scheduler or a general cgroup guarantee for descendants deliberately detaching
from a PTY; use an appropriate service account/container supervisor for those.
