# Project storage, inbox and generation

`kiokud` is one Go process: HTTP API, one generation worker, reconciliation/maintenance and an operator-only Unix socket. Only one server may write a deployment. Its SQLite process lock is local to the host; fence the old server before starting a replacement against the same object storage.

## SQLite and project storage

SQLite stores accounts, encrypted provider connections, organizations, membership, invitations, delegated credentials and project configuration/bucket/root. Project content lives in the selected object storage. The store joins configuration with an in-memory view of that content.

```text
projects/<project-id>/
  .kioku/inbox/<id>.md       # verbatim fetched body, temporary until consumed/GC
  wiki/index.md
  wiki/log.md
  wiki/source-<slug>.md
  wiki/*.md
  maps/graph.json
  maps/graph.mmd
  .kioku/state.json
  .kioku/jobs/<job-id>.json
  .kioku/audit/<event-id>.json
```

Content schema 2 stores inbox metadata, job state, scheduling deadlines and current wiki metadata. Each inbox item has an ID, source_url, status, submitter and optional run ID. Pending/removal items have no body. Fetched bodies are written before the inbox state commits; a crash in between leaves an unreferenced object for GC. The state contains no provider tokens. Recent job/audit indexes are bounded to 256 entries; unfinished jobs and pending cleanup remain indexed. Archived generation jobs remain queryable/retryable through the authorized API.

Updates confined to content do not write SQLite. Changes spanning configuration and content use a configuration-only SQLite redo intent referring to external content transaction objects. Recovery completes that intent before serving. A failed/ambiguous storage commit fails closed until restart; missing or corrupt project state is never replaced by an empty wiki.

## Put and inbox

POST `/api/projects/{id}/put` accepts exactly one of `{"url":"https://…"}`, `{"file":{"name":"doc.md","content":"…","source_url":"https://…"}}`, or `{"remove":true,"source_url":"https://…"}`. These create pending, fetched and removed items respectively. It returns 202 with `id` and `status`, not a job. File frontmatter may supply source_url; the caller's body is retained verbatim and cannot override the server-owned status. Source URLs must be HTTPS and cannot name private address literals. There is no server-side source fetcher.

`kioku put [FILES...]` sends one request per file and saves each successful receipt immediately. With no arguments it sends changed files in local inbox/. `kioku put URL` saves pending; `kioku delete URL` saves removed. Human, write-agent and MCP callers may submit URLs. Neither operation starts a runner. `GET /api/projects` includes inbox_count and an inbox list (source_url and status only) in each project entry, including pinned items; empty inboxes return 0 and []. `kioku status` prints those fields from the same response; pull and MCP read only the published wiki/maps.

Each put request is at most 1 MiB including JSON overhead (413 on excess). Each project has at most 200 inbox items, including pinned items (429 on excess). A new put replaces waiting items with the same exact source_url, including a removal canceling pending/fetched. Pinned items stay in their current run; new submissions wait for a later run. Replaced/consumed bodies are unreferenced and GC may delete them. No source history or intake journal is retained outside the published wiki.

## One current wiki

Generation atomically pins all eligible waiting items to one run before reading bodies. The input tar contains JOB.md, inbox/<id>.md with server-owned outer YAML and verbatim submitted bodies, and the entire existing wiki/. No provider tokens enter this tar. Expanded input is limited to 256 MiB. A large combined inbox/wiki can exceed this limit even when individual puts are valid; the run fails and retains items instead of silently dropping input.

The runner fetches pending items first, edits source and concept/entity/analysis/overview pages, and returns the complete wiki/*.md set. Existing sources are identified by exact frontmatter source_url and keep their filenames. New slug collisions use -2, -3, etc. The index omits source pages; log.md records changes and fetch failures, newest first, up to 100 entries. Fetch failure is per item, not a run failure: successful publication consumes it and the user must put it again to retry fetching. These semantic rules and the ban on tokens/signed URL secrets in log.md are prompt rules.

At output PUT, the server checks archive paths/types, duplicates, UTF-8, index.md, compressed/expanded size and source invariants: (a) source_url is a nonempty YAML string, (b) source URLs are unique, (c) removed URLs are absent, (d) old sources survive unless removed, (e) every fetched item has a source page. A 400 response contains a problems list and permits repair/re-upload. No per-page byte limit or page-count limit remains. Finalization checks the accepted output hash, reads its pages without repeating the validator and derives maps from their links. It does not synthesize or rewrite source pages.

There is one current wiki/maps tree, updated in place. File hashes avoid unchanged writes and omitted files are deleted. Readers can observe partial updates. `wiki_incomplete` is set before writes and cleared only when publication completes. Publication rechecks membership and the requesting agent's validity. Inbox items are consumed only in the successful final state update; new items arriving during the run stay waiting. Failure releases pinned items, except when a newer waiting item has already superseded the same URL. A partial publication retains the items and incomplete flag for recovery.

`--max-wiki-bytes` defaults to 64 MiB and measures published `Revision.StoredBytes`, including derived maps. Publication may exceed it. While over limit, pending/fetched puts return 413, removed puts remain allowed, and generation pins only removals. Without removals, it makes no model call. Generation uses the last completed publication's size, retained in `Revision.CompleteBytes` while the tree is incomplete. Partial writes crossing the limit therefore cannot block retry of the retained inbox; a wiki already over limit before the attempt still permits only removals during recovery. New pending/fetched puts remain limited by the current `StoredBytes` during recovery. Compressed and expanded output archives each have a hard limit of twice the configured value (400). That hard bound applies to the submitted wiki archive; generated maps still count in the published total.

## Scheduling and freshness

`--generate-every 24h` is the default. Persisted deadlines are checked every two seconds; waiting inbox items or incomplete publication trigger work, and an existing queued/running generation is not duplicated. Frequent puts do not postpone the deadline. `--generate-every 0` disables scheduled generation.

`kioku generate` requests a run; `generate --wait` waits and pulls. An empty inbox completes unchanged without starting a model. Missing runner configuration fails a generation job, including an empty one. `job wait ID` pulls a completed generation; `job retry ID` waits for prior cleanup and pins the items waiting at retry time. There is no full/incremental mode, fingerprint, source version or intake dependency list. Status reports inbox items, wiki_updated_at and wiki_incomplete.

## Generation runner type

`KIOKU_RUNNER` selects `claudemanaged`, `openai` or `exec`. When unset, all three of `ANTHROPIC_API_KEY`, `KIOKU_MA_AGENT_ID` and `KIOKU_MA_ENVIRONMENT_ID` enable Managed Agents; partial settings fail startup. With no configured runner, the server can accept inbox items and serve a wiki, but generation fails. There is no deterministic fallback. OpenAI requires `OPENAI_API_KEY` and `KIOKU_OPENAI_MODEL`. Exec requires the command below. Unknown names fail startup.

Generation uses the operator's runner configuration. Personal generation keys require self-hosting. Do not change runner type until running attempts and cleanup finish.

Each attempt persists Job.Run with attempt ID, pinned inbox IDs, phase, budget, deadline and job token identity before external resources are created. Start returns a runner-neutral ref. Recovery uses the same configured runner and attempt metadata; the runner type is not persisted.

## Source credentials and network policy

Managed runners create one vault per attempt, with one credential per token/destination. All values use runner.Secret. Injection is header-only: KIOKU_JOB_TOKEN to the Kioku host, GITHUB_TOKEN_<account> to api.github.com, and GOOGLE_ACCESS_TOKEN to docs.googleapis.com and www.googleapis.com. Neither the model's input tar/JOB.md/first message nor logs/API responses contain token values. The first message may contain repository-to-placeholder names, which are not secrets. Refresh tokens and GitHub App keys never enter a vault.

Immediately before Start, kiokud selects the project owner's connections. It refreshes Google's access token even if the stored token has not yet expired, and never refreshes it during the run. GitHubTokenProvider resolves only the owner's installations, intersects them with pinned pending URLs and issues tokens with repository_ids and contents:read, metadata:read, issues:read, splitting at 500 repositories per token. The provider persists issuance intent before sending. Known tokens are encrypted before entering the vault. Lost issuance responses are retained until JWT expiry + one hour + 60 seconds; expiry is not proof of revocation. Cleanup failure keeps the attempt pending for retry. Google access tokens expire; cleanup removes the vault without disconnecting the owner's grant.

The server implements the GitHub App provider, disabled by default. Each repo has token, unauthenticated or blocked mode in the first message. Unconnected or outside-installation repos use unauthenticated requests; disabled, expired authorization, suspended installations and other authorization/issuance failures are blocked. Google connection/refresh failures also omit the credential. Never fall back to another member's credential or the legacy GitHub OAuth token. See [accepted source-access risks](ACCOUNT_ADMIN.md#project-source-authorization-policy).

**The operator must configure runner networking outside Kioku.** Permit the Kioku origin, GitHub/Google API hosts above and the public websites the project needs; allow a model API only if the environment requires it. Decide how redirects, private/reserved addresses and outbound destinations are restricted in the provider environment. Kioku no longer performs server-side DNS-pinned URL fetching and does not install an allowlist. Vault host restrictions limit secret injection, not network egress. In particular, the OpenAI runner does not set a Kioku-only networking override. Disable package managers, MCP servers, web_fetch and web_search; use the approved HTTP tooling for source retrieval. The checked-in environments/kioku-wiki.yaml includes the Kioku and GitHub/Google API hosts; extend that operator-owned configuration for any additional public source hosts before using pending items. Kioku does not apply that file at startup. Validate the deployed policy before production use.

Exec receives no GitHub/Google source credential and sets fetch_allowed=false in JOB.md. It must log each pending item as a fetch failure and still process fetched/removed items. This is a prompt contract for the trusted command, not an OS network sandbox.

Provider APIs and secret isolation are covered with local fakes. A real managed sandbox, Google refresh/access, GitHub App connection and model behavior remain operator smoke-test requirements; real-service tests require an operator-created App and explicitly authorized test repositories.

## Storage selection and recovery

Select `KIOKU_STORAGE=local|s3` (or `--storage`, which takes precedence). The default is local, storing objects in `<KIOKU_DATA>/objects` (`./data/objects` when the data directory is also unset). S3 credentials with local selected are rejected. S3 requires all credentials and writes directly to remote storage without a local content cache or fallback. Existing S3 deployments must explicitly select s3. Changing this setting does not migrate existing objects or catalog bucket mappings. Old cache files are unused.

Queued/running generation jobs live in project storage. Generation has a persisted `--agent-job-timeout` (default one hour), with up to four sessions globally (`--agent-max-sessions`) and one running generation per project. The worker starts a run and proceeds to another project; there are no intake workers.

Generation phases are `preparing`, `agent_running`, `output_received`, and `finalizing`. The server validates output at upload and stores its hash; finalization pins that output against replacement. Only the reconciler calls `Runner.Inspect`, once per job per pass, and passes its `Status` to finalization, failure handling and cleanup. Passes run every 30 seconds, at worker startup, and on signed MA webhooks or exec exit callbacks. The four states are None, Running, Stopped and Unknown; the zero value is Unknown.

For a missing run handle, `Inspect` searches by job and attempt metadata and saves any recovered handle before continuing. A None result waits until five minutes after preparation, then fails with `session creation did not finish`. A recovered running run continues normally. A Stopped run with accepted output is published regardless of its stop reason, unless an `AbortReason` already exists. Without output it fails. Unknown or Running waits until the deadline, then failure handling revokes the token, records `AbortReason` and requests `Stop`. A later pass confirms the stop and records the final cost; unknown cost is omitted (`cost_cents` is absent), not recorded as zero. Persistent inspection or stop failures keep the job running until its token deadline (job deadline plus ten minutes), after which it fails with unknown cost. Unavailable runner type configuration fails a running job at its job deadline and defers cleanup until configuration is restored.

Completed/failed jobs retain a cleanup marker and remain in the hot index until resource cleanup and tar removal finish. A late-discovered running run is stopped before cleanup. Retry waits for cleanup and creates a new attempt; discovery matches that attempt ID, so previous attempts cannot supply its run or cost. Stop confirmation and publication can take until the next pass (up to 30 seconds).

Project deletion interrupts active sessions, records known usage, revokes credentials and removes temporary archives before deleting job records. If reconciliation has not confirmed stop/cleanup, deletion returns 503 and preserves the data needed to retry. URL removal instead queues a removed item and does not cancel a running generation. New puts remain independent of a running agent. `--max-jobs 20` bounds queued jobs per project; the separate inbox limit is 200.

Finishing a generation and recording its provider cost use the control/content redo protocol; page writes may already be visible before completion. Maintenance removes unreferenced inbox bodies. The server validates returned archives and never executes their contents.

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
  "project_id": "p_01",
  "input_url": "https://kioku.example/api/projects/p_01/jobs/j_01/agent/input",
  "output_url": "https://kioku.example/api/projects/p_01/jobs/j_01/agent/output",
  "job_token": "<secret>",
  "model": "claude-opus-5-5",
  "first_message": "Job j_01 for project p_01. ...",
  "prompt": "You maintain a team wiki ...",
  "budget_cents": 1000,
  "deadline": "2026-11-07T12:00:00Z"
}
```

`prompt` is the system prompt from `prompts/kioku-wiki.md`; `model` is its frontmatter model as a hint. `first_message` describes the pinned inbox and current wiki input. There is no mode field and no source credential in this contract. Fetch the gzip tar input with GET `input_url` and upload the complete gzip tar output with PUT `output_url`, using `Authorization: Bearer <job_token>` and `Kioku-Client-Version: 0.1.0` for both. The server validates the output and never executes its contents.

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

Before dequeuing new jobs after restart, the worker runs reconciliation once. The exec runner reports lost attempts as stopped with `exec run lost on restart`; jobs without output fail and lose their job tokens. `output_received` and `finalizing` resume publication from accepted output, with unknown cost omitted. Existing abort reasons always take precedence. Retry waits for cleanup and starts a new attempt.

An exec run cannot be rediscovered after restart. Keep the same runner configuration to reconcile it and remove its leftover working directory under the same data directory.

## Configuration-only online backup

Run an operator backup while the service remains available:

```bash
kiokud --data /var/lib/kioku --online-backup /var/backups/kioku/config-2026-10-31
```

The running process exposes only `DATA/admin.sock` (0600), configurable with `--admin-socket`. No public API can download a deployment backup. The destination must be a new absolute directory writable by the service account. Only one backup runs at a time. A consistent account/config snapshot is captured under the store lock; the standalone SQLite file is built afterwards without holding that lock across backup I/O. Backup files are private (0600). `backup.json` is written last with the capture timestamp, schema version and SHA-256 of the standalone SQLite file; an incomplete directory without it is not a completed backup.

The older `--backup DIR` operator command works with a stopped server and opens configuration only; it does not require S3 availability. It refuses unfinished transactions until the configured server completes recovery.

These commands create local, unencrypted configuration backups. Continuous replication of the catalog, such as Litestream to a separate bucket, credential provisioning and restore drills are operator work outside this repository. Keep the application encryption key and backup recovery credentials separately. If `KIOKU_ENCRYPTION_KEY` is unset, the server creates `DATA/encryption.key` (0600) once and reuses it; this file is not included in SQLite backups and must be backed up and restored separately. An explicit environment key takes precedence and is not copied to disk. Restore SQLite with the same S3 endpoint/access and stored project bucket mapping; the newest project content/checkpoints load from S3, not the older backup. Old working cache files can be discarded. In local-development mode without S3, `DATA/objects/` is the durable content store and must be preserved separately. Account recovery still has the configuration backup's recovery point.

SQLite backups do not restore old wiki page contents. Published source pages and any independent object-store backup policy are separate. There is no cross-project content snapshot or page-history restore promise.

## Legacy project content

Content schema 1 cannot be served by this version. The operator must stop kiokud and manually delete old project content before starting the new version. Upgrade the server, CLI and plugin together, then ask users to put their sources again.

See [implementation decisions and test coverage](INBOX_IMPLEMENTATION.md) for the explicit interpretations and remaining live verification.

### GitHub attempt authorization and recovery

The control catalog holds the owner/version for every managed attempt, including
attempts with no token. Reconnect, disconnect, GitHub identity unlink and account
delete invalidate the owner's attempts; project transfer invalidates only that
project. The same project publication lock covers invalidation. Check the binding
when saving issuance, before Start and at every publication write. A user-token
refresh is serialized per owner and records intent before POST; restart or a lost
rotation response clears the user credential and requires reconnect. No POST is
retried automatically. Installation/repository authorization reads all pages;
partial page results never authorize a token. Failures in one installation leave
other installations usable. Renamed/transferred URLs must be resubmitted when
they no longer match the current repository names.

A separate 30-second cleanup worker operates even with App settings disabled or
removed and never waits for runner Stop. Known tokens authenticate their own
DELETE /installation/token. Only 204 confirms revocation; 401 means unusable,
not necessarily expired. 403/429/5xx remain pending. Retry-After/reset and
persisted exponential secondary-limit delays survive restarts. Terminal tokens
lose ciphertext; records are removed after cleanup and aggregation. Unknown
issuance has no recoverable token and waits for conservative expiry.

`kioku_github_api_total` measures server requests directly. The separate
`kioku_github_fetch_report_total` counts model reports from the current attempt's
pinned inbox IDs in wiki/log.md (success/403/404/expired/other/unreported), once in
the publication transaction. Old logs are not recounted. Counters persist in the
catalog independently of wiki retention. Labels contain fixed classifications,
never repo names, user IDs, URLs or secrets. Cleanup/unknown age, authorization
age, selected repository count, selection results, list pages and minimum observed
user/installation rate budgets support operations. See [App operations](GITHUB_APP.md).
