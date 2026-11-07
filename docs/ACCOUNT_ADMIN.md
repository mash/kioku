# Account administration

These commands use a human login. Agent credentials cannot call them. Sensitive mutations revalidate the current identity with the OAuth provider and confirm that the login session still exists inside the transaction. If an older login has no identity proof, sign in again.

```bash
kioku account status
kioku account sessions
kioku account revoke-session SESSION_ID
kioku account revoke-all --yes
kioku account disconnect google
kioku account unlink --yes github
kioku share transfer --yes ACCOUNT_ID_OR_EMAIL
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
- If the owner cannot read a required document, its import must fail and wiki generation depending on that import must fail. The system must not fall back to another member's credentials to make the operation succeed.
- Retry follows the same owner-authorized access policy. Restricting URL retries to the original submitter is not the intended remedy for security finding F7.

This decision concerns access to external source documents. It does not grant a non-member project access or change how generation runners are authenticated and paid for. Uploaded text/files are already supplied to Kioku; an attached source URL does not itself cause a provider fetch or prove the owner's access to the original document.

### Implementation status

As inspected at commit `dede359` on 2026-11-07, URL intake still selects `providerCredential(ctx, job.UserID, provider)` in [`processPut`](../internal/server/intake.go), and [`retry`](../internal/server/pipeline.go) replaces `job.UserID` with the retrying user. Owner-based credential selection is therefore **not yet implemented**. Documenting the accepted sharing risk does not certify that the current implementation enforces the owner-only access boundary. This documentation change makes no runtime change.

Security review disposition: the sharing risk described by F7 in the local report `CLAUDE-SECURITY-20261103-162645/CLAUDE-SECURITY-RESULTS.md` is accepted under the owner-authorized model above. The historical finding remains evidence of the current credential-selection behavior; it is not marked as a completed owner-authorization fix.
