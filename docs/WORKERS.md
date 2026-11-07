# Project storage, intake and generation

`kiokud` is one Go process: HTTP API, four URL intake workers by default, one generation worker, maintenance and an operator-only Unix socket. Only one server may write a deployment. Its SQLite process lock is local to the VPS; fence the old server before starting a replacement against the same S3 buckets.

## SQLite and project storage

SQLite stores accounts, OAuth credentials, organizations, membership, invitations, delegated credentials, and project configuration/bucket/root. Sources, file locations, jobs, intake, generation status/cursors and wiki metadata are not stored in SQLite. The store joins configuration with a disposable in-memory view of project content.

Each private project bucket is independently authoritative for content:

```text
projects/<project-id>/
  sources/<source-id>/extracted.md
  wiki/index.md
  wiki/*.md
  wiki/inbox/<sequence>-<entry-id>.json
  maps/graph.json
  maps/graph.mmd
  .kioku/state.json
  .kioku/jobs/<job-id>.json
  .kioku/audit/<event-id>.json
```

`state.json` contains source metadata, URL heads, scheduling deadlines, source and wiki timestamps/versions, processing cursor and current file metadata. It contains no account credentials or membership. Recent job/audit indexes are bounded to 256 terminal/recent entries and the intake view to 64 entries; unfinished jobs remain indexed. Full intake segments and job/audit records remain in object storage. Archived job IDs remain queryable/retryable through the authorized project API. Source deletion invalidates older jobs so retry cannot resurrect canceled intake.

Updates confined to content do not write SQLite. Changes spanning configuration and content use a configuration-only SQLite redo intent referring to external content transaction objects. Recovery completes that intent before serving. A failed/ambiguous storage commit fails closed until restart; missing or corrupt project state is never replaced by an empty wiki.

## Put and intake

`kioku put [FILES...]` validates and durably stores extracted Markdown and original source URLs without generating wiki. Original files are never stored. Files require an original HTTPS URL via `--source-url` (one explicit file), `source_url` YAML frontmatter, or the API file field `source_url`. With no arguments it submits changed files from the local input inbox. `kioku put URL` records a durable URL job and returns before retrieval. Its worker uses the submitter's stored provider connection; delegated agents remain file-only. Source count, byte and queue limits still apply. A failed retrieval is visible on its job.

Successful submissions produce service-owned, append-only intake segments. Source references include contributor, timestamp, source URLs, metadata and extracted-text object keys. Submitted URL events precede retrieval; stored-source events describe actual material. Retries use stable event IDs, and existing entries cannot be modified. Generation cannot write the reserved intake paths.

`kioku pull` exposes the recent journal as `wiki/inbox.md`, including before the first generation. This is a generated reading view of the latest 64 entries, not the canonical log. Full segments remain unchanged in project storage. Extracted text is available through each entry's authenticated source link. Deleted sources are not included in this view.

`put --wait` waits for intake only.

## One current wiki

There is one current wiki/maps tree, updated in place. Generation compares file hashes and uploads changed files only, then removes obsolete generated files. It does not create a second build directory or retain page history. Readers may observe partial updates while generation runs; file metadata is recorded as each write completes.

`wiki_incomplete` is set before writes and cleared only after all writes/deletions finish. Failure leaves this flag and the last completed source coverage intact, so retry cannot incorrectly skip a partial update. Source changes during generation remain dirty for a later run. Publication rechecks project membership, agent validity and canceled/deleted input before every file mutation.

The current file descriptor and the API's legacy `revision` field are synchronization metadata, not retained build history. Pull computes a content token including the intake view; a put can therefore update the local intake view even when generated pages have not changed. The CLI keeps one current local copy, briefly stages a download, checks for user edits, and removes retired local copies after switching.

## Scheduling and freshness

`kiokud --generate-every 24h` is the default. Project deadlines persist in S3 and are checked every two seconds. Only changed projects or pending URL intake schedule generation, and an already queued/running generation is not duplicated. Continuous puts never postpone a deadline. Set another interval or `--generate-every 0` for manual generation only.

`kioku generate` requests an earlier check; `generate --wait` waits and pulls. It waits for URL puts pending at acceptance; failed dependencies fail the job without a model call. Except for explicit `generate --full`, both manual and scheduled generation skip unchanged active source IDs, preserving wiki files and timestamps even if model settings changed. Empty projects complete unchanged without a model call. `job wait ID` pulls for generation jobs; `job retry ID` retries the failed operation.

Source `updated_at` and project `sources_updated_at` advance only for material active-source changes. Identical puts/refreshes do not advance them. An older URL fetch cannot replace a newer successful observation or dirty the active sources. `wiki_updated_at` describes generated page updates, and `processed_intake_sequence` records the journal position captured by the last successful/no-op generation. Version numbers track coverage rather than wall-clock comparisons. Status exposes these timestamps, cursors and the incomplete flag.

## Generation runner type

`KIOKU_RUNNER` selects the operator's LLM generation runner type. Leave it unset to keep the existing behavior: `ANTHROPIC_API_KEY`, `KIOKU_MA_AGENT_ID` and `KIOKU_MA_ENVIRONMENT_ID` together enable Managed Agents; with none configured, generation produces a deterministic source wiki. Partial Managed Agents settings fail startup. An explicit `KIOKU_RUNNER=claudemanaged` requires all three settings. Set `KIOKU_RUNNER=exec` for a trusted external command as described below. Unknown runner names fail startup. Changing `KIOKU_RUNNER` while operator-key jobs run makes those jobs fail; wait until running jobs finish before switching. BYOK jobs are not affected.

BYOK jobs always use the owner's Managed Agents connection, regardless of the operator's runner type selection. Production uses only BYOK keys for LLM generation and leaves `ANTHROPIC_API_KEY` empty.

Each attempt persists `Job.Run` (JSON `run`) with attempt ID, phase, budget, deadline and job token identity before creating external resources. `Start` returns the runner-neutral `ref`, which is saved after creation. Runner selection uses the job’s BYOK connection and current configuration; the runner type is not persisted. Fingerprints include source IDs and hashes.

## Storage selection and recovery

Select `KIOKU_STORAGE=local|s3` (or `--storage`, which takes precedence). The default is local, storing objects in `<KIOKU_DATA>/objects` (`./data/objects` when the data directory is also unset). S3 credentials with local selected are rejected. S3 requires all credentials and writes directly to remote storage without a local content cache or fallback. Existing S3 deployments must explicitly select s3. Changing this setting does not migrate existing objects or catalog bucket mappings. Old cache files are unused.

Queued/running jobs live in project storage. URL and deterministic generation jobs return to their queues at startup. URL jobs have a two-minute deadline and deterministic generation has a five-minute execution timeout. LLM generation uses a Runner for its runner type lifecycle and has a persisted `--agent-job-timeout` (default one hour), with up to four sessions globally (`--agent-max-sessions`) and one running generation per project. The worker starts the run and immediately proceeds to another project; intake jobs use their separate workers.

Generation phases are `preparing`, `agent_running`, `output_received`, and `finalizing`. The server validates output at upload and stores its hash; finalization pins that output against replacement. Only the reconciler calls `Runner.Inspect`, once per job per pass, and passes its `Status` to finalization, failure handling and cleanup. Passes run every 30 seconds, at worker startup, and on signed MA webhooks or exec exit callbacks. The four states are None, Running, Stopped and Unknown; the zero value is Unknown.

For a missing run handle, `Inspect` searches by job and attempt metadata and saves any recovered handle before continuing. A None result waits until five minutes after preparation, then fails with `session creation did not finish`. A recovered running run continues normally. A Stopped run with accepted output is published regardless of its stop reason, unless an `AbortReason` already exists. Without output it fails. Unknown or Running waits until the deadline, then failure handling revokes the token, records `AbortReason` and requests `Stop`. A later pass confirms the stop and records the final cost; unknown cost is omitted (`cost_cents` is absent), not recorded as zero. Persistent inspection or stop failures keep the job running until its token deadline (job deadline plus ten minutes), after which it fails with unknown cost. Unavailable runner type configuration fails a running job at its job deadline and defers cleanup until configuration is restored.

Completed/failed jobs retain a cleanup marker and remain in the hot index until resource cleanup and tar removal finish. A late-discovered running run is stopped before cleanup. Retry waits for cleanup and creates a new attempt; discovery matches that attempt ID, so previous attempts cannot supply its run or cost. Stop confirmation and publication can take until the next pass (up to 30 seconds).

Source/project deletion takes the generation lock before the project lock, interrupts active sessions, records known usage, revokes credentials and removes temporary archives before deleting job records. Deleting a source, project or Anthropic connection with a running generation first returns 503 while the reconciler confirms the stop. Retry the deletion after a pass. Pending cleanup or unavailable runner type configuration also returns 503 and preserves the data needed to retry. New puts remain independent of a running agent; only the captured source versions are marked processed by its publication. Queue caps are separate: `--max-intake-jobs 200`, `--intake-workers 4`, `--max-jobs 20`. The default project source limit remains 200 snapshots/64 MiB. These bounds are capacity limits, not a sustained-throughput promise independent of quotas or upstream latency.

Finishing a generation and recording its provider cost use the control/content redo protocol; page writes may already be visible before completion. Abandoned-upload recovery runs independently of generation. Managed Agents execute in a provider container whose outbound network is restricted to kiokud. The server validates returned archives and never executes their contents.

## External runner (self-host and development)

The exec runner type starts one trusted command for each generation attempt. Production does not use it. The command runs as the kiokud OS user and can read that user's files and process environment, including server credentials. The command must provide its own sandbox and credential isolation; a separate process is not a sandbox. This repository does not include a runner command.

Set both variables and keep the command's executable path absolute:

```sh
KIOKU_RUNNER=exec
KIOKU_RUNNER_COMMAND='["/usr/local/bin/kioku-gen", "--config", "/etc/kioku-gen.json"]'
```

`KIOKU_RUNNER_COMMAND` is a JSON array of strings passed directly as argv, without a shell. Put the command's provider credentials and settings in a file referenced by an absolute argv path, or in its wrapper script. Do not put credentials in argv. kiokud passes only `PATH` from its own environment, `HOME` set to the attempt's working directory, and `TMPDIR` set to that directory's `tmp` subdirectory. It does not pass LLM keys, `DOCKER_HOST`, or the job token as environment variables.

Startup validates the command array and that argv[0] is executable, the `KIOKU_BASE_URL` origin, and the embedded prompt's model, then creates `<DATA>/exec-runs` with mode 0700. Each attempt uses `<job-id>-<attempt-id>` and its `tmp` subdirectory, also mode 0700. `DATA` is the `--data` directory (default `./data`). A missing or non-executable command prevents kiokud from starting. BYOK selects `claudemanaged` regardless of these operator settings.

### JSON contract v1

The command receives one JSON object on stdin, followed by EOF:

```json
{
  "version": 1,
  "job_id": "j_01",
  "attempt_id": "a1b2c3",
  "project_id": "p_01",
  "mode": "incremental",
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

`mode` is `incremental` or `full`. `prompt` is the system prompt from `prompts/kioku-wiki.md`; `model` is its frontmatter model as a hint. `first_message` describes the captured generation input. Fetch the gzip tar input with GET `input_url` and upload the complete gzip tar output with PUT `output_url`, using `Authorization: Bearer <job_token>` for both. The server validates the output and never executes its contents.

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

SQLite backups do not restore old wiki page contents. Original/source retention and any independent S3 backup policy are separate. There is no cross-project content snapshot or page-history restore promise.

## Legacy migration

On first startup, the old per-project SQLite layout and metadata JSON are migrated into the configured project object storage. A private, complete `DATA/migration-backup/` archive preserves the old SQLite files before changing the catalog to schema 2. Legacy metadata JSON is retained as `.migrated`. Active project SQLite files are removed only after content is durable and the configuration schema change commits. Retain/protect these migration artifacts until verified; they contain old account/content metadata and are not part of new configuration-only backups.

Only the selected legacy wiki is copied into the current tree; historical page records/directories are removed. Missing legacy files fail startup. Migration does not copy an old shared S3 bucket into new dedicated buckets; configure/copy that storage explicitly before upgrading. A storage read/write migration failure must be resolved before serving traffic.
