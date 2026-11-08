# Account administration

These commands use a human login. Agent credentials cannot call them. Sensitive mutations revalidate the current identity with the OAuth provider and confirm that the login session still exists inside the transaction. If an older login has no identity proof, sign in again.

```bash
kioku account status
kioku account sessions
kioku account revoke-session SESSION_ID
kioku account revoke-all --yes
kioku account unlink --yes github
kioku project share transfer --yes ACCOUNT_ID_OR_EMAIL
kioku org transfer --yes ORGANIZATION ACCOUNT_ID_OR_EMAIL
kioku account delete --yes
```

Session listings contain opaque revocation IDs, expiry and provider names, not bearer tokens or provider credentials. `revoke-all` invalidates all of the account's current sessions, outstanding bound login flows, and all its delegated agent credentials. It does not prevent a later legitimate OAuth sign-in.

`unlink` removes a login identity, revokes sessions issued through that provider and revokes all delegated agent credentials. The last login identity cannot be removed. To unlink a provider, sign in through another linked provider first.

Project ownership can be transferred to another existing project member; the previous owner becomes a member. Organization ownership can be transferred to an existing organization/project member. Organization ownership does not implicitly grant project-content access.

Account deletion requires explicit confirmation and refuses to orphan owned projects or nonempty organizations. Transfer/delete owned projects and resolve organization ownership first. Empty organizations can be removed with the account. Deletion removes the account profile, login identities and memberships, revokes sessions/agents and unused invitations created by the account, and preserves other members' project data. Historical source attribution and audit references remain; this is not a request to erase shared project documents.

There is no unauthenticated recovery endpoint or account-merge shortcut. If all linked provider accounts are lost, recovery needs an operator to verify ownership outside the service before a separate administrative migration. No endpoint permits account recovery based solely on an unverified email address.

## Project source authorization policy

The user's Claude fetches each source with the user's own connectors or CLI. The client sends the text and a source_url to Kioku. Human sessions, write agents and MCP clients can submit text. Kioku stores the text as a fetched inbox item and shares the published source pages with project members.

source_url is client-asserted provenance. The server validates the URL format but does not verify that the text came from that URL or that the submitter can read the original. A member can submit different text under an existing source_url. A later submission replaces an earlier waiting item with the same exact URL, even if the later text is older.

Kioku holds no source-access credentials and never retrieves source text. GitHub and Google OAuth authorize login only. Login credentials remain available for identity verification during account administration. Runners receive the submitted text and process fetched and removed items. Managed attempts exchange input and output files through provider APIs.
