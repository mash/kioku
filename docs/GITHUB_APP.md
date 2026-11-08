# GitHub App source access

One GitHub App handles both login and source access, with separate authorization
flows and credential storage. Login uses the App Client ID and client secret;
`GET /user` supplies the numeric identity ID, and `GET /user/emails` supplies the
verified primary email required for invitations and account administration.
Login tokens stay in encrypted session identity proofs, never in source
connections or runner credentials. Source access requires `connect github` and
uses installation tokens restricted to inbox repositories. Google-authenticated
Kioku users can connect it too. This integration supports **github.com only**.

GitHub login is available only with complete App settings. The source-access
switch defaults to disabled and does not disable login. With App settings unset,
Google login still works when configured.

The [user authorization web flow](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/generating-a-user-access-token-for-a-github-app)
uses the existing state, PKCE and CSRF checks without OAuth scopes. App registration
permissions control access; [Email addresses: read](https://docs.github.com/en/rest/authentication/permissions-required-for-github-apps#user-permissions-for-email-addresses)
permits `GET /user/emails` (documentation checked 2026-11-08).

Login identity proofs retain the access-token expiry and refresh token. Before
rechecking email for invitations or account administration, Kioku refreshes an
expiring token and saves the rotated credentials for every session from that
login before fetching identity data. Concurrent checks share that refresh;
separate logins and source connections retain their own credentials. A failed
refresh requires signing in again. See GitHub's
[refresh-token lifecycle](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/refreshing-user-access-tokens).

## Create the App (operator)

In github.com, open **Settings → Developer settings → GitHub Apps → New GitHub
App** under the account or organization that will own the App. Decide that owner
before registration. The table uses production origin `https://kioku.magicians.ee`;
for another deployment replace that origin everywhere with `KIOKU_BASE_URL`.
The App owner is an operator decision; this repository does not register an App.

| GitHub setting | Exact value |
|---|---|
| GitHub App name | `Kioku Readonly Sources` (append a deployment name if taken) |
| Description | `Sign in to Kioku and read selected GitHub source repositories.` |
| Homepage URL | `https://kioku.magicians.ee` |
| Callback URLs (register both, in this order) | `https://kioku.magicians.ee/auth/github/callback`<br>`https://kioku.magicians.ee/auth/github-app/callback` |
| Expire user authorization tokens | **Enabled** |
| Request user authorization (OAuth) during installation | **Disabled** |
| Enable Device Flow | **Disabled** |
| Setup URL | `https://kioku.magicians.ee/auth/github-app/setup` |
| Redirect on update | **Enabled** |
| Webhook Active | **Disabled** |
| Webhook URL / Secret / subscribed events | **Empty / none** |
| Repository permissions → Contents | **Read-only** |
| Repository permissions → Metadata | **Read-only** (GitHub requires it) |
| Repository permissions → Issues | **Read-only** |
| Repository permissions → Pull requests | **No access** |
| All other Repository permissions | **No access** |
| Organization permissions | **All No access** |
| Account permissions → Email addresses | **Read-only** |
| All other Account permissions | **No access** |
| Where can this GitHub App be installed? | **Any account** |

Keep **Request user authorization (OAuth) during installation Disabled**: login
and source connect start their own authorization flows with explicit callback
URLs; installation returns to the Setup URL and does not establish a connection.

After creating the App, copy its **App ID**, **Client ID**, and the actual slug
from its public App URL. Generate a **client secret** and an **RSA private key**.
Keep them out of chat, Git, CLI configuration and runner environments. Put the
PEM key in a server-readable **0600** regular file, mounted read-only into kiokud.
Configure its absolute path as seen by the process. PKCS#1 and PKCS#8 RSA keys
of at least 2048 bits are accepted. Keep server time synchronized.

```dotenv
GITHUB_APP_ENABLED=false
GITHUB_APP_ID=APP_ID
GITHUB_APP_CLIENT_ID=CLIENT_ID
GITHUB_APP_SLUG=ACTUAL_REGISTERED_SLUG
GITHUB_APP_CLIENT_SECRET=SECRET_FROM_OPERATOR_SECRET_STORE
GITHUB_APP_PRIVATE_KEY_FILE=/run/secrets/kioku-github-app.pem
```

These five App settings must be complete together and supply both login and source
access. No separate login app is needed. Callback and setup origins come from
`KIOKU_BASE_URL`. Preserve `KIOKU_ENCRYPTION_KEY`, or the generated `<KIOKU_DATA>/encryption.key`, separately
from the App private key. User credentials and cleanup tokens in SQLite require
the same database encryption key after restart or restore.

| Startup configuration | Result |
|---|---|
| All five settings empty, enabled=false | Starts with Google login configured; GitHub login unavailable; cleanup continues |
| Complete settings, enabled=false | GitHub login available; no new source connections/issuance; cleanup continues |
| Complete settings, enabled=true | GitHub login available; recovers durable records before accepting new source work |
| Partial settings, invalid key, or enabled=true with no settings | Refuses startup |
| Stored App credentials cannot be decrypted | Refuses startup; restore the database encryption key |

## Connect and recover (project owner)

Use the checkout configured for the intended project; confirm the server origin.

```sh
kioku -C /absolute/path/to/project account status
kioku -C /absolute/path/to/project connect github
kioku -C /absolute/path/to/project account status
```

The browser first authorizes the GitHub user, then lists that user's eligible App
installations. Install/update the App if needed, choosing the required accounts
and repositories. No installation is selected by default. Select the desired
installations and return to the same CLI, signed into the **same Kioku session**.
The ticket lasts 10 minutes; the final single-use code lasts one minute. GitHub's
setup `installation_id` is never accepted as proof of access. Installation alone
does not establish a Kioku connection. Organization approval can require a fresh
`connect` after the administrator approves the request.

`account status` shows `github_app.configured`, `enabled`, `status`, the GitHub
login, and each installation's account/status/checked_at. These are stored last
observations, not a live GitHub lookup. `whoami` uses the same `connections`
projection. A reconnect replaces the old connection only after CLI completion;
cancelled/failed flows preserve it. It does not change login identity or profile.

| Result | Action |
|---|---|
| Not configured / disabled | Ask the operator to configure/enable the App |
| needs_installation | Install it for the account/repositories, then reconnect |
| approval_required | Ask the organization administrator, then reconnect |
| reauthorization_required | Run `connect github`; a lost refresh response is not retried |
| suspended / inaccessible | Check suspension, selected repos and the user's current organization access; reconnect if needed |
| Session changed / expired | Log in and start a new connect; another session cannot finish this ticket |
| Connection version conflict | A different connect/disconnect won; start a fresh connect |
| 403 / rate_limited / temporary | Check the recorded reason and retry later; no permission expansion or fallback |
| 404 | Could be absent or inaccessible; it does not prove a private repo is absent |
| Rename / transfer | Put the current canonical URL again if the old URL no longer matches |

After resolving an individual fetch failure, **the project owner** connects at
that directory's server, then the user resubmits the original URL with
`kioku -C /absolute/path/to/project put URL`. A completed generation has consumed
the original inbox item. Use `generate --wait` and `pull` if an immediate result
is requested. A member's separate GitHub connection cannot substitute for the
owner's. Remote MCP has no App-management tools.

`kioku account disconnect --yes github` invalidates old attempts and clears user
and staged credentials. Its response includes `cleanup_pending`. Known issued
tokens are revoked independently of runner shutdown; the user or organization
administrator must uninstall the App separately if desired. Source token access
is repo-wide, not limited to the one submitted file. Reconnect invalidates all of
the owner's old-version attempts; owner transfer affects only the transferred
project. Refresh does not change connection_version.

## Rollout and existing data

Production data was wiped before this change, so no additional migration of old
login rows is needed. Identity lookup still uses the provider name and numeric
GitHub user ID; changing the app type does not change that key. The existing
one-time cleanup of legacy source credentials remains for other catalogs.

1. Locate the deployment's actual environment/secret mounts and agent update
   mechanism. This checkout does not supply the production deployment settings.
   Back up `catalog.sqlite` through the supported backup command and preserve its
   encryption key. Verify restore on an isolated copy with the same binary.
2. Register a test App using the settings above. Install it on selected public and
   private fixture repositories. Record the operator's App owner and actual slug.
3. Ship the complete implementation, initially with enabled=false. The
   first startup removes old GitHub source ciphertext, pending source-connect
   tickets/OAuth states, and pre-migration GitHub session identity credentials.
   Sessions themselves and login identities remain. The migration marker is
   durable and later App login identity proofs are not cleared again.
4. Update the Anthropic agent to the new `prompts/kioku-wiki.md` and pin its agent
   version. Ship the OpenAI server binary containing that same prompt. Permit
   `api.github.com` in both sandbox network policies. App secrets/user tokens stay
   exclusively on kiokud; only installation tokens enter host-limited vaults.
5. Enable the App and restart. Have the owner connect; inspect account status.
6. With explicit real-service test authorization, fetch public/private README,
   blob, Issue, PR description and first-page comments in **both** managed
   runners. Include a file larger than 1 MiB and verify truncation. Check that a
   write request in a disposable fixture repository is denied and an unsubmitted
   repository is outside scope. Test separate owners, uninstall/suspend, repository
   deselection and organization departure. Verify secret isolation, publication,
   session/vault archive/deletion and token cleanup. Record live results separately
   from local fake tests. Do not claim this smoke test ran merely because CI passed.

## Failure, monitoring and key rotation

Source-connect user access/refresh tokens are encrypted separately from login
identity proofs and never enter a runner. Source access tokens are refreshed with less than five minutes remaining; refresh has a durable intent
and no automatic resend. JWT uses RS256, iss=client_id, iat=now−60s, exp=now+9m.
REST calls use `X-GitHub-Api-Version: 2026-03-10`, a 10-second deadline and no
redirects. An issuance request always specifies nonempty repository IDs (at most
500) and Contents/Metadata/Issues read permissions.

Known installation tokens retain their API expiry. DELETE's 204 means revoked;
401 means unusable, not proof of expiry. 403/429/5xx retry after persisted delays.
Unknown issuance cannot be immediately revoked: it remains in expiry_wait until
JWT.exp + 1 hour + 60 seconds, then closes as expired_unobserved. This is a
conservative time bound, **not** a confirmed GitHub revocation. Cleanup remains
active with the App disabled or its settings removed because known tokens revoke
themselves without an App signing key. Preserve the database encryption key.

Monitor fixed-label `kioku_github_connect_total`, `token_total`, `api_total`,
`credential_selection_total`, `fetch_report_total` (each with the
`kioku_github_` prefix), `kioku_github_scope_repositories`, list page counts,
cleanup/unknown counts and ages, authorization age and minimum observed user /
installation rate remaining. Fetch reports are model observations from the current
attempt's item IDs; missing, malformed or duplicate reports count as unreported.
Counters are in the catalog and survive wiki log retention. No repo/user/URL/token
is a metric label. Webhooks are deliberately disabled: GitHub-side changes are
checked before issuance and do not produce immediate Kioku notifications.

For planned key rotation: generate another App key; install the new PEM with 0600
permissions and update the path; restart and validate test issuance; then delete
the old key in GitHub. During compromise, disable new connections/issuance, let
known-token cleanup run, replace the compromised client secret/key as appropriate,
and reconnect affected users. Deleting an App key alone does not confirm that
already issued tokens were revoked.

Prefer a forward fix. For rollback, disable new work, wait for runner and token
cleanup (including unknown expiry), stop kiokud, then restore a compatible catalog
backup and its encryption key. **Never open a catalog containing the new record
kinds with the old binary**: its save path can discard unfamiliar records. Restoring
a pre-upgrade backup loses later account/configuration changes and does not restore
old wiki content; review those consequences before rollback.
