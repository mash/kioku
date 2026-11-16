# Project storage, inbox and generation

`kiokud` is one Go process: HTTP API, one generation worker, and reconciliation/maintenance. Only one server may write a deployment. Its SQLite process lock is local to the host; fence the old server before starting a replacement against the same object storage.

## SQLite and project storage

SQLite stores accounts, encrypted login credentials, organizations, membership, invitations, delegated credentials and project configuration/bucket/root. Project content lives in the selected object storage. The store joins configuration with an in-memory view of that content.

```text
projects/<project-id>/
  .kioku/inbox/<id>.md       # verbatim submitted body, temporary until consumed/GC
  wiki/index.md
  wiki/log.md
  wiki/source-<slug>.md
  wiki/*.md
  .kioku/state.json
  .kioku/jobs/<job-id>.json
  .kioku/audit/<event-id>.json
```

Content schema 2 stores inbox metadata, job state, scheduling deadlines and current wiki metadata. Each inbox item has an ID, source_url, optional remove flag, submitter and optional run ID. Items with remove true have no body. The server writes submitted bodies before committing the inbox state; a crash in between leaves an unreferenced object for GC. The state contains no provider tokens. Recent job/audit indexes are bounded to 256 entries; unfinished jobs and pending cleanup remain indexed. Archived generation jobs remain queryable through the authorized API.

Updates confined to content do not write SQLite. Changes spanning configuration and content use a configuration-only SQLite redo intent referring to external content transaction objects. Recovery completes that intent before serving. A failed/ambiguous storage commit fails closed until restart; missing or corrupt project state is never replaced by an empty wiki.

## Put and inbox

POST `/api/projects/{organization}/{project}/put` accepts one file or one removal:

```json
{"file":{"name":"doc.md","content":"---\nsource_url: https://example.com/doc\nupdated_at: 2026-11-07T18:00:00+09:00\n---\nDocument text"}}
{"remove":true,"source_url":"https://example.com/doc"}
```

A file creates an item without the remove flag. A removal creates an item with `remove: true`. The server returns 202 with `{}`. It does not create a job.

Every file must start with a YAML mapping delimited by complete `---` lines (LF or CRLF; the closing line may end at EOF). Empty blocks and duplicate keys are rejected. It must contain `source_url` and `updated_at`. The URL must use public HTTPS and pass `ValidateSourceURL`. Write `updated_at` as an RFC 3339 timestamp with an explicit offset, such as `+09:00` or `Z`. Use the original document's last-updated time. If the original has no update metadata, use the fetch time. Invalid or missing frontmatter causes a 400 response naming frontmatter; missing, empty or invalid provenance values cause a 400 response naming `source_url` or `updated_at`.

The server reads provenance only from the file. It rejects `file.source_url` and a top-level `source_url` with a file. The top-level `source_url` is for removals only. The server validates updated_at but keeps it only in the unchanged submitted body, not in inbox metadata. The body cannot override the server-owned remove flag. The sender asserts provenance. The server does not retrieve the URL or verify the source of the text.

`kioku put FILE...` requires at least one file or directory path and sends one request per file. It prints `accepted <path>` for each successful request and never deletes source files. Each file needs the same frontmatter. There is no `--source-url` flag. `kioku delete URL` submits a removal. MCP `source_put` takes `name` and `content` for a file. It takes `remove: true` and `source_url` for a removal. Do not send the `source_url` argument with `content`. These operations do not start a runner.

`GET /api/projects` includes inbox_count and an inbox list (source_url and remove true only for removals) in each project entry. The list includes pinned items. Empty inboxes return 0 and []. `kioku status` prints these fields. Pull and the MCP Wiki tools read published Wiki pages.

Each put request is at most 1 MiB including JSON overhead (413 on excess). Each project has at most 200 inbox items, including pinned items (429 on excess). A new put replaces waiting items with the same exact source_url, including a removal canceling a submitted body. Pinned items stay in their current run; new submissions wait for a later run. Replaced/consumed bodies are unreferenced and GC may delete them. No source history or intake journal is retained outside the published wiki.

## One current wiki

Generation atomically pins all eligible waiting items to one run before reading bodies. The input tar contains JOB.md, WIKI.md, inbox/<id>.md with server-owned outer YAML and verbatim submitted bodies, and the entire existing wiki/. JOB.md contains the job ID and project namespace. Exec also receives the input and output URLs in JOB.md. WIKI.md contains the Wiki View's saved prompt verbatim, or the default Wiki prompt when none is saved. The runner follows it when choosing, splitting and structuring pages and wiki/index.md; conflicting system rules take precedence. Instructions and output rules are in the system prompt. The outer YAML contains source_url, submitter and remove true only for removals. The runner copies updated_at from the submitted file frontmatter inside the item body into the source page YAML frontmatter and uses source times to date facts. Nested frontmatter remains source content; only the outer YAML controls the remove flag. No provider tokens enter this tar. Expanded input, including WIKI.md, is limited to 256 MiB. WIKI.md also counts toward OpenAI's 50 MiB compressed input limit. A large combined inbox/wiki/prompt can exceed this limit even when individual puts are valid; the run fails and retains items instead of silently dropping input.

The runner uses the remove flag to process submitted bodies and removal requests, edits source pages and organizes other pages according to WIKI.md, and returns the complete wiki/*.md set. Existing sources are identified by exact frontmatter source_url and keep their filenames. New slug collisions use -2, -3, etc. The index omits source pages; log.md records changes, newest first, up to 100 entries. The prompt requires these content rules and forbids tokens and signed URL secrets in log.md.

At output PUT, the server checks archive paths/types, duplicates, UTF-8, index.md, compressed/expanded size and source invariants: (a) source_url is a nonempty YAML string, (b) source URLs are unique, (c) removed URLs are absent, (d) old sources survive unless removed, (e) every item without remove true has a source page. A 400 response contains a problems list and permits repair/re-upload. No per-page byte limit or page-count limit remains. Finalization checks the accepted output hash, reads its pages without repeating the validator and derives maps from their links. It does not synthesize or rewrite source pages.

There is one current Wiki tree. Publication replaces it atomically in one catalog transaction. File hashes avoid unchanged writes and omitted files are deleted. Publication rechecks membership and the requesting agent's validity. Inbox items are consumed only in the successful final state update; new items arriving during the run stay waiting. Failure releases pinned items, except when a newer waiting item has already superseded the same URL.

`--max-wiki-bytes` defaults to 64 MiB and measures published Wiki bytes in `Revision.StoredBytes`. Publication may exceed it. While over limit, file puts return 413, removal puts remain allowed, and generation pins only removals. Without removals, the Wiki stage makes no model call. Compressed and expanded Wiki output archives each have a hard limit of twice the configured value (400).

## Scheduling and freshness

The debounce period (`--generate-debounce`, default 10s) restarts at each saved put. When it ends and no job is queued or running, kiokud queues an automatic generation for waiting inbox items. kiokud does not retry a failed automatic generation. With a debounce period of 0, kiokud generates at the next check.

`kioku generate` requests a run; `generate --wait` waits and pulls. An empty inbox completes unchanged without starting a model. Missing runner configuration fails a generation job, including an empty one. `job wait ID` pulls a completed generation; Run `kioku generate --wait` again after a failure. The new job pins the items waiting when it starts. There is no full/incremental mode, fingerprint, source version or intake dependency list. Status reports inbox items and wiki_updated_at.

## Generation runner type

`KIOKU_RUNNER` selects `claudemanaged`, `openai` or `exec`. When unset, all three of `ANTHROPIC_API_KEY`, `KIOKU_MA_AGENT_ID` and `KIOKU_MA_ENVIRONMENT_ID` enable Managed Agents; partial settings fail startup. With no configured runner, the server can accept inbox items and serve a wiki, but generation fails. There is no deterministic fallback. OpenAI requires `OPENAI_API_KEY` and `KIOKU_OPENAI_MODEL`. Exec requires the command below. Unknown names fail startup.

Generation uses the operator's runner configuration. Personal generation keys require self-hosting. Do not change runner type until running attempts and cleanup finish.

Each attempt persists its ID, pinned inbox, phase, budget and deadline before it creates external resources. Managed attempts also persist their provider, transport, and resource IDs. Exec persists its job token identity. Recovery uses the saved provider and attempt metadata.

Project generation gives the Wiki the full requested budget first, then generates the project's other Views in creation order: oldest `created_at` first, ties by `view_id` in string order. Views without `created_at` follow those with it, in slug order. Web and MCP lists use the same order; the Wiki View uses the project's creation time and comes first. After each attempt the server subtracts its reported cost from the remaining budget. If the runner cannot report a settled cost, that attempt leaves the remaining allowance unchanged. OpenAI does not currently report a settled per-session cost through this integration, so the total bill can exceed the generation request's amount.

The Wiki and each other View run in separate runner sessions under one project generation job. Each non-Wiki View session receives the complete Wiki and its saved prompt in `VIEW.md`. It returns a gzip-compressed tar archive with `index.html` at the root and optional images and CSS under `assets/`, plus optional `summary.md` and `data.json`. Kioku validates those files before writing the View ZIP.

The Wiki View has reserved slug `wiki` and default title `Wiki`. Owners can edit its title and prompt through the View settings and API. Saving settings does not regenerate the Wiki; the prompt applies when a later generation builds the Wiki runner's input archive. An empty inbox does not start the Wiki runner or read its prompt. POST `/api/projects/{organization}/{project}/views` rejects explicit slug `wiki` with 409 and `view slug is already in use`, even without saved Wiki settings. Omitting the slug with title `Wiki` assigns `view-<hash>`. DELETE of the Wiki View returns 409 and `the wiki view cannot be deleted` after authorization checks.

GET and PATCH responses for the Wiki View derive `status` from the published Wiki: `completed` when a Wiki exists; otherwise `pending`. Only `completed` responses include `generated_at`, using `wiki_updated_at`. `wiki_updated_since_run` is null; `error`, `job_id` and `wiki_snapshot_id` are omitted. The Wiki View outputs `wiki/*.md` and has no bundle. MCP `view_list` includes it first and returns no prompts for any View. `view_read` with `view:"wiki"` returns the tool error `the wiki view has no bundle; read it with wiki_read or wiki_search`. Its Web page shows status, update time and a link to the Wiki body instead of an HTML preview.

Editing any View's title or prompt during generation does not change its generation status or skip publication. Each View uses the settings at its input-archive build time: edits before that point apply to this run, and edits after it apply to the next run. Deleting a View during generation skips it with `View が削除済み`, including when a new View reuses its slug with a different `view_id`. A failed View stops later Views, marking them `skipped` with `前の View が失敗しました`; a skipped View does not stop later Views. A failed View deleted before its result is recorded reports `View が削除済み` and does not stop later Views. Missing or incomplete Wiki and a remaining budget below 50 cents also skip Views. Wiki failure fails the project job and prevents other Views from running. The job's `views` contains only non-Wiki Views; `wiki_status` reports the Wiki stage.

## Prompt configuration

`KIOKU_PROMPTS_DIR` optionally replaces `wiki-agent.system.md`, `wiki-agent.user.md`, `wiki-agent.default-prompt.md`, and the description fields in `mcp-server.yaml`. The server reads and validates these once before constructing runners or accepting HTTP requests. Missing files use embedded defaults; unknown visible filenames, invalid templates/YAML, and unknown keys stop startup. See [the configuration reference](../README.md#生成プロンプトと-mcp-の説明の差し替え) for variables, model handling, and examples.

The default Wiki prompt is UTF-8 text with a leading BOM removed, no frontmatter and no template expansion; strings such as `{{.BaseURL}}` remain literal. It must contain non-whitespace text. An empty body stops startup with `prompts: wiki-agent.default-prompt.md: empty body (line 1)`; a leading `---` stops startup with `prompts: wiki-agent.default-prompt.md: syntax (line 1)`. An owner-saved Wiki prompt always takes precedence over the default.

Startup logs record `file`, `source=default|override` and `sha256` for all four files, including `wiki-agent.default-prompt.md`, without exposing content. The system prompt uses deployment variables only; per-job variables are confined to the user template. `JOB.md` is not customizable. MCP descriptions and optional `instructions` are served to clients, not to the generation agent; no `listChanged` notification is sent.

All Wiki runners receive this fixed paragraph immediately after the rendered system, including an overridden system:

> WIKI.md in the input holds the project owner's instructions for organizing this wiki. Follow them when you choose, split and structure pages and wiki/index.md. When they conflict with the rules above, follow the rules above.

File-based runners append their fixed input/output instructions after this paragraph. Exec includes the paragraph in its JSON `prompt`.

MA sessions always receive `agent_with_overrides`: rendered system, model when present, the fixed Go tool list, and empty non-nil `mcp_servers` and `skills`. `KIOKU_MA_AGENT_VERSION` still pins the referenced agent version. The MA system limit is 100,000 Unicode runes, checked at startup after adding the fixed paragraph, including when MA is selected implicitly. Agent resources carry the placeholder documented in README; environment resources must be separate for each deployment. OpenAI uses the rendered system with `KIOKU_OPENAI_MODEL`. Exec receives the rendered system and user message on stdin.

Existing MA/OpenAI sessions retain their original prompts and input archive, including WIKI.md, across restart. Discovery of an uncertain start resumes that session without creating another one; a new generation uses current prompts. Exec stops on restart. MA session-create 4xx errors log `session create rejected status=... type=...`; 429 remains temporary, while other 4xx fail the attempt. Error conversion retains only the HTTP status and SDK error type, never the provider message or request.

## Job credential and network policy

Managed generation uses only attached files and provider APIs. Kioku uploads the
input archive with an expiry, attaches it to the session, and retrieves the
output after successful completion. New managed attempts do not issue a job
token or attach a vault. The client retrieves source text before submission.

Create the Claude environment from [environments/kioku-wiki.yaml](../environments/kioku-wiki.yaml).
Kioku reads back the policy before starting the session. It requires a cloud
environment with `limited` networking, an empty allowlist, and no MCP or package
manager exceptions. Kioku also clears inherited MCP servers and skills, disables
web fetch and web search, and pins the inspected single-agent version. Manage
the environment per deployment and avoid modifying it while the deployment runs.

OpenAI uses a hosted environment with networking disabled. Kioku confirms the
effective policy before sending the first turn. The input is limited to 50 MiB
compressed, and an oversized input fails without dropping any inbox items.
Use provider projects or workspaces dedicated to the corresponding trust
boundary before attaching real tenant data. In Anthropic, uploaded files are
workspace-scoped even when their mounted copies are session-scoped.

Exec processes the same submitted text over its authenticated HTTP contract.
The trusted command enforces its own network sandbox.

## Storage selection and recovery

Select `KIOKU_STORAGE=local|s3` (or `--storage`, which takes precedence). The default is local, storing objects in `<KIOKU_DATA>/objects` (`./data/objects` when the data directory is also unset). S3 credentials with local selected are rejected. S3 requires all credentials and writes directly to remote storage without a local content cache or fallback. Existing S3 deployments must explicitly select s3. Changing this setting does not migrate existing objects or catalog bucket mappings. Old cache files are unused.

Queued/running generation jobs live in project storage. Generation has a persisted `--agent-job-timeout` (default one hour), with up to four sessions globally (`--agent-max-sessions`) and one running generation per project. The worker starts a run and proceeds to another project; there are no intake workers.

Generation phases are `preparing`, `agent_running`, `output_received`, and `finalizing`. The server validates output on retrieval or upload and stores its hash; finalization pins that output against replacement. Only the reconciler calls `Runner.Inspect`, once per job per pass, and passes its `Status` to finalization, failure handling and cleanup. Passes run every 30 seconds, at worker startup, and on signed MA webhooks or exec exit callbacks. The four states are None, Running, Stopped and Unknown; the zero value is Unknown.

For a missing run handle, `Inspect` searches by job and attempt metadata and saves any recovered handle before continuing. A None result waits until five minutes after preparation, then fails with `session creation did not finish`. A recovered running run continues normally. A file-based run must finish successfully. Kioku then waits up to 90 seconds, bounded by the job deadline, for its session or turn to publish the expected output. Ambiguous or invalid outputs fail the attempt. For the existing HTTP contract, accepted output can be published after the run stops unless it was aborted. Unknown or Running waits until the deadline, then failure handling revokes the token, records `AbortReason` and requests `Stop`. A later pass confirms the stop and records the final cost; unknown cost is omitted (`cost_cents` is absent), not recorded as zero. Persistent inspection or stop failures keep the job running until its token deadline (job deadline plus ten minutes), after which it fails with unknown cost. Unavailable runner type configuration fails a running job at its job deadline and defers cleanup until configuration is restored.

Completed/failed jobs retain a cleanup marker and remain in the hot index until resource cleanup and tar removal finish. A late-discovered running run is stopped before cleanup. Each generation has a new job and attempt ID. Discovery matches both IDs, so a previous generation cannot supply its run or cost. Stop confirmation and publication can take until the next pass (up to 30 seconds).

Project deletion interrupts active sessions, records known usage, revokes credentials and removes temporary archives before deleting job records. If reconciliation has not confirmed stop/cleanup, deletion returns 503 and preserves the data needed to retry. URL removal instead queues an item with remove true and does not cancel a running generation. New puts remain independent of a running agent. `--max-jobs 20` bounds queued jobs per project; the separate inbox limit is 200.

Finishing a generation and recording its provider cost use the control/content redo protocol; page writes may already be visible before completion. Hourly maintenance prunes expired sessions, invites and old history, and removes unreferenced inbox bodies. The server validates returned archives and never executes their contents.

## External runner (self-host and development)

The exec runner type starts one trusted command for each generation attempt. Production does not use it. The command runs as the kiokud OS user and can read that user's files and process environment, including server credentials. The command must provide its own sandbox and credential isolation; a separate process is not a sandbox. This repository does not include a runner command.

Set both variables and keep the command's executable path absolute:

```sh
KIOKU_RUNNER=exec
KIOKU_RUNNER_COMMAND='["/usr/local/bin/kioku-gen", "--config", "/etc/kioku-gen.json"]'
```

`KIOKU_RUNNER_COMMAND` is a JSON array of strings passed directly as argv, without a shell. Put the command's provider credentials and settings in a file referenced by an absolute argv path, or in its wrapper script. Do not put credentials in argv. kiokud passes only `PATH` from its own environment, `HOME` set to the attempt's working directory, and `TMPDIR` set to that directory's `tmp` subdirectory. It does not pass LLM keys, `DOCKER_HOST`, or the job token as environment variables.

Startup validates the command array and that argv[0] is executable, the `KIOKU_BASE_URL` origin, and the embedded prompt's model, then creates `<DATA>/exec-runs` with mode 0700. Each attempt uses `<job-id>-<attempt-id>` and its `tmp` subdirectory, also mode 0700. `DATA` is the `--data` directory (default `./data`). A missing or non-executable command prevents kiokud from starting.

### JSON contract v1

The command receives one JSON object on stdin, followed by EOF:

```json
{
  "version": 1,
  "job_id": "j_01",
  "attempt_id": "a1b2c3",
  "input_url": "https://kioku.example/api/projects/acme/platform/jobs/j_01/agent/input",
  "output_url": "https://kioku.example/api/projects/acme/platform/jobs/j_01/agent/output",
  "job_token": "<secret>",
  "model": "claude-opus-5-5",
  "first_message": "job: j_01\nnamespace: acme/platform\ninput: https://...\noutput: https://...\n",
  "prompt": "You maintain a team wiki ...",
  "budget_cents": 1000,
  "deadline": "2026-11-07T12:00:00Z"
}
```

For Wiki generation, `prompt` and `first_message` are rendered from `prompts/wiki-agent.system.md` and `prompts/wiki-agent.user.md` or their `KIOKU_PROMPTS_DIR` overrides, with the fixed WIKI.md paragraph appended to `prompt`. For non-Wiki View generation those fields describe the View archive contract and point to the saved prompt in `VIEW.md`. The configured Wiki model ID is passed as a hint in `model`. A model map uses only `id`, with a startup warning when other keys are set. The input archive's `JOB.md` contains the job metadata. Fetch the gzip tar input with GET `input_url` and upload the complete gzip tar output with PUT `output_url`, using `Authorization: Bearer <job_token>` and `Kioku-Client-Version: 0.1.0` for both. Kioku validates the archive before publication.

The prompt refers to `$KIOKU_JOB_TOKEN`. The command must make that name usable within its sandbox. Outside development, inject the real value into kiokud-bound headers through a proxy so the model and tool output never see it. Use a separate sandbox for every run, allow outbound access only to kiokud and the required LLM API, and disable package-manager networking, MCP, web fetch and web search. Keep provider keys, object-store credentials and other projects' tokens outside the sandbox. Remove the sandbox and its secrets afterwards.

The command enforces `budget_cents` (USD cents) and `deadline`. The common server limits still apply: a minimum budget of 50 cents, `--agent-max-session-usd`, `--agent-max-sessions`, and `--agent-job-timeout`. The worker's five-minute startup context does not govern the child's lifetime; the reconciler stops it after the persisted deadline.

Write a result object as stdout's last nonempty line:

```json
{"version":1,"reason":"","cost_cents":137}
```

`cost_cents` is a self-reported integer from 0 through 1,000,000 inclusive. Missing, null, out-of-range or non-integer cost, invalid JSON, or a version other than 1 makes the result invalid. Unknown cost is represented by an absent `cost_cents` in `Job.Run`. `invalid result line` is logged at warning level for exit code 0, and debug otherwise. The run handle contains only the attempt ID, with no host paths.

Exit code 0 with a valid result means a normal stop. A nonzero exit or invalid result means a stopped run with the result's reason, or `agent failed` when no reason was supplied. A signal following `Stop` means `terminated`. Exit zero only when generation is finished. The reconciler publishes already accepted output from any stopped run regardless of its reason; an existing abort reason takes precedence over output.

`reason` must contain no secrets. kiokud keeps at most 512 bytes in `Status.Reason`, which can become `Job.Error` and is visible in the job API. It retains the last nonempty stdout line, up to 1 MiB; an oversized line is invalid. For stderr it keeps only a byte count, logged at debug level. Stderr contents are never retained, logged or copied into the job error.

### Stop, shutdown and restart

Stopping sends SIGTERM to the process group and returns immediately. After ten seconds, a still-running child receives SIGKILL. On SIGTERM the command must remove any external resources it created, such as remote sandboxes or containers, within that grace period. Cleanup kills and waits for the child, then kills the group again even after a successful exit to remove remaining descendants. It removes the attempt's working directory only within the configured exec root. A cleanup timeout remains pending for the next reconciliation.

On normal shutdown, kiokud cancels exit-triggered reconciliation, stops its children, and waits up to fifteen seconds before closing the store. On Linux, the direct child also receives SIGKILL if its creating OS thread dies; this does not cover descendants. A crash may leave descendants, and on other operating systems direct children, running. External resources can survive SIGKILL or a crash. Only one kiokud may own a deployment; fence the old instance before starting another against the same project storage.

Before dequeuing new jobs after restart, the worker runs reconciliation once. The exec runner reports lost attempts as stopped with `exec run lost on restart`; jobs without output fail and lose their job tokens. `output_received` and `finalizing` resume publication from accepted output, with unknown cost omitted. Existing abort reasons always take precedence.

An exec run cannot be rediscovered after restart. Keep the same runner configuration to reconcile it and remove its leftover working directory under the same data directory.

## Catalog backup

kiokud has no backup command. Continuous replication of `catalog.sqlite`, such as Litestream to a separate bucket, is operator work outside this repository. For a one-off copy while the service runs, use `sqlite3 DATA/catalog.sqlite "VACUUM INTO '/new/path/catalog.sqlite'"`.

Credential provisioning and restore drills are operator work outside this repository. Keep the application encryption key and backup recovery credentials separately. If `KIOKU_ENCRYPTION_KEY` is unset, the server creates `DATA/encryption.key` (0600) once and reuses it; this file is not included in SQLite backups and must be backed up and restored separately. An explicit environment key takes precedence and is not copied to disk. Restore SQLite with the same S3 endpoint/access and stored project bucket mapping; the newest project content/checkpoints load from S3, not the older backup. Old working cache files can be discarded. In local-development mode without S3, `DATA/objects/` is the durable content store and must be preserved separately. Account recovery still has the configuration backup's recovery point.

SQLite backups do not restore old wiki page contents. Published source pages and any independent object-store backup policy are separate. There is no cross-project content snapshot or page-history restore promise.

## Legacy project content

Content schema 1 cannot be served by this version. The operator must stop kiokud and manually delete old project content before starting the new version. Upgrade the server, CLI and plugin together, then ask users to put their sources again.

See [implementation decisions and test coverage](INBOX_IMPLEMENTATION.md) for the explicit interpretations and remaining live verification.
