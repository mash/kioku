# Project-scoped cloud agents

A Kioku project credential is an API authorization boundary. It is not an operating-system sandbox. A cloud agent with shell access can read any file its VM/container user can access, regardless of its Kioku project. Project config and directory names alone do not isolate files or credentials.

For each agent run, provision a separate VM/container or an equivalent provider-enforced sandbox. Mount only that project's checkout and inputs, give it a fresh Kioku configuration directory, and provision only that project's credential. Do not mount a developer's home, the shared `~/.config/kioku` directory, the server's catalog/databases, object-store credentials, other projects' checkouts, or the container host's control socket. Scope the runner's GitHub and other connected tools separately; Kioku cannot restrict credentials supplied by another platform. Separate processes under the same OS user are not a filesystem security boundary.

Install `kioku` in the runner and allow HTTPS access to the service. An isolated configuration directory is per-run state; remove it when the sandbox is destroyed. Do not reuse a sandbox between projects without rebuilding it and removing all prior project files and credentials.

## Claude connector

Add `https://YOUR_KIOKU_SERVER/mcp` as a custom connector in Claude. The default service uses `https://kioku.magicians.ee/mcp`. Sign in to Kioku with Google or GitHub. The connector supplies its own display name, so check the address that will receive the authorization code. If you recognize it, check the projects you want to share and click **Allow**. No projects are checked by default. If you have none, create or join a project first.

The consent page asks you to trust the destination to read and search the wikis, add sources, and issue kioku CLI login codes for the projects you select. The connector keeps the OAuth grant. The local or cloud CLI does not need access to a developer's home directory.

The connector sees only the projects that you chose at consent. Every tool applies the same check: the project must be in the grant and you must still be a member. For a project outside the grant, the tools return the same `project not found` text as for a project where you are not a member. To give the connector another project, reconnect and select it again.

The MCP endpoint exposes five tools:

- `cli_login_code`: issue a one-time code for CLI login to a known `organization/project` namespace.
- `projects_list`: list the projects that you chose at consent and where you are still a member.
- `wiki_read`: read a current wiki or map file; follow `next_offset` to read long files.
- `wiki_search`: find a case-insensitive substring in the current wiki and maps.
- `source_put`: accept one URL, one UTF-8 text file, or one removal and return its inbox `id` and `status`.

`source_put` accepts exactly one of these shapes (with the selected `project`):

```json
{"project":"acme/docs","url":"https://github.com/acme/docs/blob/main/design.md"}
{"project":"acme/docs","name":"design.md","content":"Original text","source_url":"https://example.com/design"}
{"project":"acme/docs","remove":true,"source_url":"https://example.com/design"}
```

The statuses are `pending`, `fetched` and `removed`. The normalized put request is limited to 1 MiB including JSON overhead. A file can instead carry `source_url` in YAML frontmatter. The tool saves an inbox item; it does not fetch the URL or start generation. The scheduler includes it in a later run. Waiting items with the same exact URL are replaced, while items pinned to an active run remain unchanged.

```text
user ──> claude.ai chat ──(skill kioku loads from the plugin)
              │
              │ MCP tools/call over HTTPS, Bearer = connector OAuth access token
              ▼
         kiokud POST /mcp
              ├─ projects_list            → granted projects where the user is a member
              ├─ wiki_read / wiki_search  → current wiki/ and maps/ files (read-only)
              ├─ source_put               → stores one pending/fetched/removed inbox item
              └─ cli_login_code           → one-time code for kioku agent login
```

For CLI login, supply the known `organization/project` namespace to `cli_login_code`. Set `role: "write"` for uploads or generation; the default is `read`. The server returns a one-time code, its expiry, and a command only if you selected that project at consent and are still a member. Run the returned command immediately in the selected project directory:

```bash
export KIOKU_SERVER=https://kioku.magicians.ee
cd /path/to/project
kioku agent login CODE
kioku whoami
kioku pull
```

`CODE` is the `kioku_code.…` value returned by the tool. It expires in **60 seconds** and works once. If it expires or has already been used, request a fresh code. Do not echo it or save it elsewhere. It passes through the model's context, but the resulting agent token does not: the CLI exchanges it directly and prints only `id`, `role`, `expires`, and `namespace`.

`agent login` requires an explicit `KIOKU_SERVER` or `--server` and uses cwd (or `-C`). It saves the server and namespace in `.kioku/config` and refuses a different existing binding. It writes the **24-hour** agent credential atomically with mode `0600` at `~/.config/kioku/agents/<server-hash>/<organization>/<project>.json`. It never puts a token in the project config or changes human credentials in `~/.config/kioku/auth/`.

When a human credential exists, every command uses it, and `agent login` fails without using the code. Otherwise, commands use the agent credential. An invalid or expired one fails before any request (except `logout`, which accepts an expired one), and a revoked one gets a 401; run `agent login` with a new code, which replaces the old credential of that hostname. `kioku logout` revokes the selected credential and, for an agent credential, removes its file. The CLI rejects `KIOKU_AGENT_TOKEN`.

The server checks the grant's project selection and your current membership again when the CLI exchanges the code. For example, if you belong to `acme/docs` and `acme/infra` but select only `acme/docs`, the connector cannot read, search, add sources to, or issue a code for `acme/infra`. Joining another project or refreshing the OAuth token does not add it. To authorize another project, reconnect and select the projects for the new grant.

A grant that has no project selection, such as a grant from before project selection existed, gives access to no projects. The server rejects every MCP request for that grant with `connector login required` and every token refresh with `invalid_grant`, so the connector must reconnect.

Only a **human OAuth grant** can delegate these credentials; an **agent credential cannot issue credentials**. OAuth access tokens authenticate only `/mcp`, never the ordinary API. Access tokens last one hour, refresh tokens last 30 days, and refresh tokens rotate on use.

Authorizing a new grant does not revoke earlier grants or issued agent credentials. From a human CLI session, `kioku account revoke-all --yes` disables the user's connector access/refresh tokens, pending codes, and agent credentials. Account deletion is the other account-wide kill switch. There is no connector grant management UI. Use the existing `agent revoke` command to revoke one issued agent; this does not revoke the connector's human OAuth grant. The VM/container isolation requirements above still apply.

Manage issued agent credentials with `kioku agent list` and `kioku agent revoke AGENT_ID`. Each project permits at most 100 active credentials. A member can manage their own credentials; project owners can revoke all credentials in that project.

## Allowed operations

| Credential | Allowed |
| --- | --- |
| Read | Its own identity metadata; only its selected project in project listings; that project's published wiki/maps, inbox metadata, jobs, and technical usage; logout of itself |
| Write | Everything above, plus URL/file/removal puts, explicit generation and retrying jobs in the same project |

Both roles are denied account administration, organization enumeration/creation, project creation/deletion, membership changes, ownership changes, issuing credentials, and Google/GitHub connections. Write agents may put URLs. Generation uses the project owner's connection; the calling agent receives no provider credentials. Use URLs for GitHub, Google Docs and public Web, or original text plus `source_url` for material the runner cannot fetch. The provenance URL of a file submission is not fetched. See the accepted sharing risks in [account administration](ACCOUNT_ADMIN.md#project-source-authorization-policy).

Published source pages are readable by project members. Writes may consume the organization's existing budget; an agent cannot increase that budget.

Expiry, revocation, issuer removal from the project, account deletion and account-wide access revocation disable the credential. Removing a member permanently revokes their existing project credentials, so rejoining does not resurrect them. Queued and running agent jobs recheck the credential before model calls and before publication. Already-running external model requests may incur provider costs; the known usage of managed sessions is recorded even when publication fails or is revoked. Revocation prevents subsequent requests; bytes already returned to an agent cannot be taken back.

Request authentication retains the original restricted principal for the entire request. A concurrent revocation never turns an agent request into the issuer's unrestricted account identity.

The server has one mutable wiki/maps tree. Pull can include partial progress while generation is running; inspect `status` for `wiki_incomplete` when freshness matters. Pull and MCP expose only published wiki/maps. Before generation, inbox metadata is available through the CLI status or each entry of GET /api/projects (inbox_count and inbox, containing only source_url and status). Project content and inbox state live in project storage, independently of the account/configuration SQLite backup.

## Internal runner credential

LLM wiki generation uses a third role, `job`, issued only by kiokud. Users cannot obtain it. It permits only GET input and PUT output for one project and one job; human sessions and read/write agents cannot use those two endpoints. The server stores only its hash and issues a fresh token expiring at the job deadline plus ten minutes. Successful completion, failure handling and deletion revoke it. The worker reconciles lost exec runs before dequeuing new jobs after a restart, revoking their job tokens when the run completes or fails.

Each managed attempt has one vault. `KIOKU_JOB_TOKEN` is injected into request headers only for the kiokud host. Owner source tokens use separate credentials in the same vault: `GITHUB_TOKEN_<account>` for `api.github.com`, and `GOOGLE_ACCESS_TOKEN` for `docs.googleapis.com` and `www.googleapis.com`. The sandbox receives placeholders, never provider token values. It receives no server API key, OAuth refresh token, GitHub App private key, object-store credential or other project's job token. Tokens stay in `runner.Secret` and never enter the input tar, JOB.md, first message, persisted job or API response.

GitHub credentials cover only the pinned inbox repositories in the owner's installations, with `contents: read`. The GitHub App provider is an interface; the shipped server has no App connection implementation and supplies no GitHub token. Public repositories use unauthenticated requests; private ones fail without stopping other items. Google access is refreshed just before Start and never during a run. Missing/expired credentials or forbidden documents are per-item fetch failures recorded in `wiki/log.md`; there is no fallback to another member's connection.

The operator configures network policy on the managed environment. Allow kiokud, the provider API hosts above and the public sites that projects need. Keep package-manager networking, MCP, web fetch and web search disabled; the prompt uses HTTP tools. No code in Kioku installs a network allowlist. Host-limited vault injection and network egress are separate controls. See [worker operations](WORKERS.md#source-credentials-and-network-policy). The trusted exec runner receives no source credentials and treats pending items as fetch failures, while processing fetched and removed items.

The runner owns its session/vault resources. Cleanup revokes the job token, removes the vault and temporary tar objects, and revokes GitHub installation tokens through the attempt-scoped provider. Failed or unavailable cleanup remains pending across restart and blocks retry until resolved. Google access tokens expire; the owner's reusable provider grant remains connected.

Source text is untrusted data. The input contains JOB.md, server-owned outer metadata for inbox items and the entire current wiki. Output contains the complete wiki/*.md set, including source pages. At upload, Kioku checks archive paths/types, duplicates, UTF-8, index.md, compressed/expanded size (twice max-wiki-bytes) and source_url invariants: every source has a nonempty string URL, URLs are unique, existing sources survive unless removed, removed sources disappear, and fetched items have a matching source. There is no fixed page count or per-page byte limit. A 400 response lists problems for the runner to repair. Finalization checks the accepted archive hash and publishes without a second source validation.

The prompt requires source citations, an index without source pages and a newest-first log capped at 100 entries. It forbids tokens and signed URL secrets in the log. These content rules are prompt instructions, not server checks. Per-item fetch failures still permit publication; run failure preserves the inbox for retry.

## Intended connector skill steps

The kioku skill in [`plugin/skills/kioku`](../plugin/skills/kioku/SKILL.md) follows these steps.

1. Choose the project cwd (or `-C`) and set `KIOKU_SERVER`. Read the expected namespace from the repository or skill. If `.kioku/config` already exists, require its server and namespace to match.
2. Run `kioku whoami` in that cwd, and choose a credential in this order. When a human credential works, use it. When no human credential exists and an agent credential for the selected project works, use the agent credential. When no credential works in a Claude Code cloud session, use connector login; the cloud session sets `CLAUDE_CODE_REMOTE_SESSION_ID`. When no credential works on a local machine, ask the human to run the login with `!` and wait. Show the exact command with absolute paths expanded, such as `! /abs/path/to/kioku -C /abs/path/to/project login`. Without the plugin, use `! kioku login`. Claude never runs `kioku login`, because login needs a browser. Where `!` is unavailable, as in Claude Desktop and Cowork, use connector login. If the human asks for connector login while a human credential exists, explain that connector login needs `kioku logout`, which ends the human session; run `logout` only after the human agrees. If `agent login` fails with `a human login exists; run kioku logout first to use connector login` and `kioku whoami` fails with an authentication error, run `kioku logout` once and start this order again. For connector login, call `cli_login_code` for the expected namespace, then run `kioku agent login CODE` immediately. Request `write` only for uploads or generation.
3. Run ordinary `kioku pull`, then grep the local `wiki/` and `maps/`. An expired or consumed code requires a new tool call; an expired agent requires a new code exchange.
4. When the CLI cannot run or reach `https://kioku.magicians.ee`, use remote mode through the kioku MCP server. Read with `wiki_read` and `wiki_search`; save sources with `source_put`. Use the namespace from the conversation or user message. Otherwise, call `projects_list`; ask the user when more than one project could match. Read `wiki/index.md` first. If the chosen project uses another server, report the local error and stop. Authentication and membership errors do not trigger remote mode. Keep the chosen mode for the conversation.
