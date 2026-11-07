# Account administration

These commands use a human login. Agent credentials cannot call them. Sensitive mutations revalidate the current identity with the OAuth provider and confirm that the login session still exists inside the transaction. If an older login has no identity proof, sign in again.

```bash
kioku account status
kioku account sessions
kioku account revoke-session SESSION_ID
kioku account revoke-all --yes
kioku account disconnect google
kioku account unlink --yes github
kioku project share transfer --yes ACCOUNT_ID_OR_EMAIL
kioku org transfer --yes ORGANIZATION ACCOUNT_ID_OR_EMAIL
kioku account delete --yes
```

Session listings contain opaque revocation IDs, expiry and provider names, not bearer tokens or provider credentials. `revoke-all` invalidates all of the account's current sessions, outstanding bound device/connect flows, and all its delegated agent credentials. It does not prevent a later legitimate OAuth sign-in.

`disconnect` removes the stored source-access connection. It preserves the login identity and does not revoke the OAuth app grant at Google/GitHub; users can manage those grants at the provider. Reconnect with `kioku connect PROVIDER`. `unlink` removes a login identity as well as its source connection, revokes sessions issued through that provider and all delegated agent credentials. The last login identity cannot be removed. To unlink a provider, sign in through another linked provider first.

Project ownership can be transferred to another existing project member; the previous owner becomes a member. Organization ownership can be transferred to an existing organization/project member. Organization ownership does not implicitly grant project-content access.

Account deletion requires explicit confirmation and refuses to orphan owned projects or nonempty organizations. Transfer/delete owned projects and resolve organization ownership first. Empty organizations can be removed with the account. Deletion removes the account profile, login identities, stored provider connections and memberships, revokes sessions/agents and unused invitations created by the account, and preserves other members' project data. Historical source attribution and audit references remain; this is not a request to erase shared project documents.

There is no unauthenticated recovery endpoint or account-merge shortcut. If all linked provider accounts are lost, recovery needs an operator to verify ownership outside the service before a separate administrative migration. No endpoint permits account recovery based solely on an unverified email address.

## Project source authorization policy

Design decision confirmed by the project maintainer on 2026-11-07:

- The project owner authorizes source import and wiki generation for the shared project. Google/GitHub document access is evaluated using the owner's authorized provider connection, not the submitter's or the person retrying a job.
- A member may submit a document URL they cannot personally read. If the owner can read it, importing it and making the resulting source and wiki available to project members is intended behavior. The owner accepts this sharing risk when authorizing import and generation.
- If the owner cannot read a document, or has no connection for its provider, fetching that item fails. The runner records its URL, reason and submitter in wiki/log.md and continues with the other items. Successful publication consumes the failed item too. The system never falls back to another member's credentials.
- Retry follows the same owner-authorized access policy. Restricting URL retries to the original submitter is not the intended remedy for security finding F7.

Google access tokens cannot be restricted to the submitted documents. Source text is untrusted, and prompt injection could make a runner read and publish an owner's document that nobody submitted. The owner also accepts this risk when enabling Google source access. Host restrictions do not enforce a document-level boundary. The prompt instructs the runner to fetch only submitted URLs, but this is not an authorization check.

GitHub credentials are scoped to the owner's installations and the repositories named by the pinned inbox URLs, with `contents: read`, `metadata: read`, and `issues: read`. There is no per-project repository permission list. A submitted repository is a token scope, not a guarantee that the runner will read only the exact submitted file. Endpoints requiring other permissions can fail; Kioku does not broaden the token to make them succeed.

This decision concerns access to external source documents. It does not grant a non-member project access or change how generation runners are authenticated and paid for. Uploaded text/files are already supplied to Kioku; an attached source URL does not itself cause a provider fetch or prove the owner's access to the original document.

### Implementation status

Generation selects the current project owner's source connection immediately before starting a managed runner. Google access tokens are refreshed at that point and not during the run; the refresh token stays in kiokud. The attempt vault receives only short-lived access tokens and the job token. Cleanup removes the vault; Google access tokens expire without revoking the owner's reusable Google grant.

GitHub plumbing is implemented behind `server.GitHubTokenProvider`, including installation selection, repository filtering, 500-repository chunks and attempt-scoped revocation. The GitHub App is disabled by default. An operator configures it, then the project owner runs `kioku connect github` and selects installations. A Google login can connect a GitHub App without changing login identities. The server intersects the owner's current GitHub user access, selected installations and pinned inbox repositories. Outside-installation or unconnected repositories use unauthenticated requests; authorization or issuance failures are blocked and never fall back. The old GitHub OAuth connection is never used. See [operator settings](GITHUB_APP.md).

Local fake-provider tests verify owner selection, refresh, scopes, cleanup after restart and secret handling. They do not verify real provider access or model compliance with the prompt. The historical F7 report describes the previous submitter-based behavior; the owner-only policy and its broader Google sharing risk above describe this implementation.

GitHub App connection status is projected consistently into `account status` and
`whoami`. `account status` also shows configured/enabled, the GitHub login, and
last-checked installation status (active/suspended/inaccessible); it does not
call GitHub. Reconnect and disconnect invalidate all old attempts of that owner.
Transferring a project invalidates only that project's attempts. Publication is
checked against the pinned owner/version. Disconnect clears encrypted user and
staging credentials, leaves a version tombstone, and returns `cleanup_pending`;
installation-token cleanup continues even if the runner cannot stop. This does
not uninstall the GitHub App. User refresh rotates a separate credential generation
without changing the connection version. An uncertain refresh requires reconnect.
