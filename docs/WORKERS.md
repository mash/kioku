# Project storage, intake and generation

`kiokud` is one Go process: HTTP API, four URL intake workers by default, one generation worker, maintenance and an operator-only Unix socket. Only one server may write a deployment. Its SQLite process lock is local to the VPS; fence the old server before starting a replacement against the same S3 buckets.

## SQLite and project storage

SQLite stores accounts, OAuth credentials, organizations, membership, invitations, delegated credentials, and project configuration/bucket/root. Sources, file locations, jobs, intake, generation status/cursors and wiki metadata are not stored in SQLite. The store joins configuration with a disposable in-memory view of project content.

Each private project bucket is independently authoritative for content:

```text
projects/<project-id>/
  sources/<source-id>/original
  sources/<source-id>/extracted.txt
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

`kioku put [FILES...]` validates and durably stores sources without generating wiki. With no arguments it submits changed files from the local input inbox. `kioku put URL` records a durable URL job and returns before retrieval. Its worker uses the submitter's stored provider connection; delegated agents remain file-only. Source count, byte and queue limits still apply. A failed retrieval is visible on its job.

Successful submissions produce service-owned, append-only intake segments. Source references include contributor, timestamp, source metadata and original/text object keys. Submitted URL events precede retrieval; stored-source events describe actual material. Retries use stable event IDs, and existing entries cannot be modified. Generation cannot write the reserved intake paths.

`kioku pull` exposes the recent journal as `wiki/inbox.md`, including before the first generation. This is a generated reading view of the latest 64 entries, not the canonical log. Full segments remain unchanged in project storage. Original bodies are available through each entry's authenticated source link. Deleted originals are not included in this view.

`put --wait` waits for intake only.

## One current wiki

There is one current wiki/maps tree, updated in place. Generation compares file hashes and uploads changed files only, then removes obsolete generated files. It does not create a second build directory or retain page history. Readers may observe partial updates while generation runs; file metadata is recorded as each write completes.

`wiki_incomplete` is set before writes and cleared only after all writes/deletions finish. Failure leaves this flag and the last completed source coverage intact, so retry cannot incorrectly skip a partial update. Source changes during generation remain dirty for a later run. Publication rechecks project membership, agent validity and canceled/deleted input before every file mutation.

The current file descriptor and the API's legacy `revision` field are synchronization metadata, not retained build history. Pull computes a content token including the intake view; a put can therefore update the local intake view even when generated pages have not changed. The CLI keeps one current local copy, briefly stages a download, checks for user edits, and removes retired local copies after switching.

## Scheduling and freshness

`kiokud --generate-every 24h` is the default. Project deadlines persist in S3 and are checked every two seconds. Only changed projects or pending URL intake schedule generation, and an already queued/running generation is not duplicated. Continuous puts never postpone a deadline. Set another interval or `--generate-every 0` for manual generation only.

`kioku generate` requests an earlier check; `generate --wait` waits and pulls. It waits for URL puts pending at acceptance; failed dependencies fail the job without a model call. Except for explicit `generate --full`, both manual and scheduled generation skip unchanged active source IDs, preserving wiki files and timestamps even if model settings changed. Empty projects complete unchanged without a model call. `job wait ID` pulls for generation jobs; `job retry ID` retries the failed operation.

Source `updated_at` and project `sources_updated_at` advance only for material active-source changes. Identical puts/refreshes do not advance them. An older URL fetch cannot replace a newer successful observation or dirty the active sources. `wiki_updated_at` describes generated page updates, and `processed_intake_sequence` records the journal position captured by the last successful/no-op generation. Version numbers track coverage rather than wall-clock comparisons. Status exposes these timestamps, cursors and the incomplete flag.

## Storage selection and recovery

Select `KIOKU_STORAGE=local|s3` (or `--storage`, which takes precedence). Backend selection is required; S3 credentials with local selected are rejected. S3 requires all credentials and writes directly to remote storage without a local content cache or fallback. Existing S3 deployments must explicitly select s3. Changing this setting does not migrate existing objects or catalog bucket mappings. Old cache files are unused.

Queued/running jobs live in project storage. URL and deterministic generation jobs return to their queues at startup. URL jobs have a two-minute deadline and deterministic generation has a five-minute execution timeout. Managed generation has a persisted `--agent-job-timeout` (default one hour), with up to four sessions globally (`--agent-max-sessions`) and one running generation per project. The worker starts the session and immediately proceeds to another project; intake jobs use their separate workers.

Managed phases are `preparing`, `agent_running`, `output_received`, and `finalizing`. The validated complete output is pinned by hash when finalization begins. A 30-second reconciler, also run at worker startup, and optional signed webhooks inspect session state and finalize or fail idempotently. Startup preserves every managed phase and its attempt ID. A preparing run with no saved session ID is searched by job metadata after five minutes, interrupted if found, and failed. A lost session-create response follows this same recovery. Finalizing resumes publication after restart. A persisted stop reason keeps the run open while a session is still observed running after interruption; its credential is revoked. Completed/failed jobs retain a cleanup marker and remain in the hot index until archive and tar removal finish, including across restart. Retry waits for this cleanup, then starts a new attempt. Session recovery also matches the attempt ID in metadata, so the cost of an older attempt never reaches the new one.

Source/project deletion takes the generation lock before the project lock, interrupts active sessions, records known usage, revokes credentials and removes temporary archives before deleting job records. If stopping or cleanup remains incomplete, deletion returns 503 and preserves the source/project so it can be retried. New puts remain independent of a running agent; only the captured source versions are marked processed by its publication. Queue caps are separate: `--max-intake-jobs 200`, `--intake-workers 4`, `--max-jobs 20`. The default project source limit remains 200 snapshots/64 MiB. These bounds are capacity limits, not a sustained-throughput promise independent of quotas or upstream latency.

Finishing a generation and recording its provider cost use the control/content redo protocol; page writes may already be visible before completion. Abandoned-upload recovery runs independently of generation. Managed Agents execute in a provider container whose outbound network is restricted to kiokud. The server validates returned archives and never executes their contents.

## Configuration-only online backup

Run an operator backup while the service remains available:

```bash
kiokud --data /var/lib/kioku --online-backup /var/backups/kioku/config-2026-10-31
```

The running process exposes only `DATA/admin.sock` (0600), configurable with `--admin-socket`. No public API can download a deployment backup. The destination must be a new absolute directory writable by the service account. Only one backup runs at a time. A consistent account/config snapshot is captured under the store lock; the standalone SQLite file is built afterwards without holding that lock across backup I/O. Backup files are private (0600). `backup.json` is written last with the capture timestamp, schema version and SHA-256 of the standalone SQLite file; an incomplete directory without it is not a completed backup.

The older `--backup DIR` operator command works with a stopped server and opens configuration only; it does not require S3 availability. It refuses unfinished transactions until the configured server completes recovery.

These commands create local, unencrypted configuration backups. Continuous replication of the catalog, such as Litestream to a separate bucket, credential provisioning and restore drills are operator work outside this repository. Keep the application encryption key and backup recovery credentials separately. Restore SQLite with the same S3 endpoint/access and stored project bucket mapping; the newest project content/checkpoints load from S3, not the older backup. Old working cache files can be discarded. In local-development mode without S3, `DATA/objects/` is the durable content store and must be preserved separately. Account recovery still has the configuration backup's recovery point.

SQLite backups do not restore old wiki page contents. Original/source retention and any independent S3 backup policy are separate. There is no cross-project content snapshot or page-history restore promise.

## Legacy migration

On first startup, the old per-project SQLite layout and metadata JSON are migrated into the configured project object storage. A private, complete `DATA/migration-backup/` archive preserves the old SQLite files before changing the catalog to schema 2. Legacy metadata JSON is retained as `.migrated`. Active project SQLite files are removed only after content is durable and the configuration schema change commits. Retain/protect these migration artifacts until verified; they contain old account/content metadata and are not part of new configuration-only backups.

Only the selected legacy wiki is copied into the current tree; historical page records/directories are removed. Missing legacy files fail startup. Migration does not copy an old shared S3 bucket into new dedicated buckets; configure/copy that storage explicitly before upgrading. A storage read/write migration failure must be resolved before serving traffic.
