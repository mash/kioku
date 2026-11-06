# Managed Agents with a user's Anthropic API key

Users connect at `/settings/anthropic`. Use a key scoped to a dedicated Claude
Console workspace. For a multi-workspace key, also provide the workspace ID.
This is separately billed API usage, not Claude Pro/Max subscription usage.

Setup creates one Agent and one Environment in that workspace using the embedded
`prompts/kioku-wiki.md` definition. The environment permits only the configured
Kioku HTTPS host. Retrying partial setup reuses saved IDs or discovers resources
by a stable connection metadata tag. Setup does not start a generation session.

Each user has one connection. The API key is encrypted with `KIOKU_ENCRYPTION_KEY`,
with authenticated context binding it to the user and connection. Keep that
server key stable and protected alongside the encrypted database backups.
The provider key is never returned by an API or given to the agent sandbox.
The existing job-scoped Kioku generator token is delivered through the job vault.

Manual generation uses the requesting user's connection. The request accepts
`budget_cents` (minimum 50, maximum the server's configured session limit).
The project page defaults to $0.50. These are provider session budgets, not a
promise of an exact final invoice. Storage limits still apply. Jobs preserve the connection ID across retries and
use it for sessions, vaults, monitoring, interruption and cleanup. A missing or
invalid connection never falls back to the operator's key.

Automatic generation is paused for projects whose owner has an Anthropic
connection. Initial BYOK supports manual generation only. Agent credentials
cannot trigger a human's BYOK usage. Only the user whose connection runs a job
can retry it.

Key updates must access the existing Agent and Environment before replacing the
stored key. Running jobs pick up the updated key. To change workspace, disconnect
first. Disconnection blocks new jobs, cancels queued jobs, and requests shutdown
and cleanup of running jobs before deleting the stored key. If the key has
already been revoked, restore a valid key for the same workspace to finish
cleanup. Never pretend that an unreachable provider session has stopped.
Agent and Environment definitions remain in the user's Console after disconnect;
per-job sessions and vaults are archived. API key revocation itself is performed
by the user in Console.

Completion uses server polling; a user-specific Anthropic webhook is not needed.
The operator's optional webhook remains available for legacy operator-paid jobs.

API endpoints (human session required):
- `PUT /api/account/anthropic`: `{"api_key":"...","workspace_id":"..."}`.
- `DELETE /api/account/anthropic`: disconnect after cleanup.
- `POST /api/projects/{project}/generate`: `{"full":true,"budget_cents":50}`.

Verification before declaring production BYOK operational:
1. Connect a dedicated real workspace and confirm Agent/Environment provisioning.
2. Generate from synthetic source documents with a $0.50 session budget.
3. Confirm published pages, provider usage, job-token revocation and vault/session cleanup.
4. Confirm the same user's job survives a server restart and valid key rotation.

Automated tests cover cross-user routing, missing-connection fail-closed behavior,
key rotation, no operator AI charge, setup recovery, budgets and disconnect failures.
Real provider behavior must also be verified; mocked tests do not establish it.

## Production verification — 2026-11-01

A real BYOK smoke test passed on the hosted service. One synthetic source
generated `wiki/index.md` and `wiki/infrastructure.md` with source links;
publication to R2 and browser rendering succeeded.

- Session budget: $0.50; Anthropic-reported list cost: $0.07.
- Provider session terminated and archived; job vault archived.
- Kioku generator token revoked; durable cleanup marked complete.
- HTTPS health and isolated catalog restore/SQLite integrity checks passed.

The real test exposed SDK request unmarshalling dropping model/tool fields.
Provisioning now sends the manifest with the SDK parameter override mechanism;
tests assert model, instructions, tool restrictions and network configuration.
Restart during an active real session and real API-key rotation remain untested;
the corresponding credential routing and recovery behavior have automated tests.
