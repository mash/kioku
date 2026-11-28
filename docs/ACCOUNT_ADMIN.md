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
kioku org add-owner --yes ORGANIZATION ACCOUNT_ID_OR_EMAIL
kioku account delete --yes
```

Session listings contain opaque revocation IDs, expiry and provider names, not bearer tokens or provider credentials. `revoke-all` invalidates all of the account's current sessions, outstanding bound login flows, and all its delegated agent credentials. It does not prevent a later legitimate OAuth sign-in.

`unlink` removes a login identity, revokes sessions issued through that provider and revokes all delegated agent credentials. The last login identity cannot be removed. To unlink a provider, sign in through another linked provider first.

A project owner can transfer ownership to another project member and remain a member. An organization owner can add a co-owner or transfer ownership to an existing organization or project member. General does not allow project ownership transfer. An organization that contains General allows adding a co-owner, but not transferring ownership. Organization owners do not gain access to projects they have not joined.

Account deletion requires explicit confirmation. Before deleting your account, delete other projects in your personal organization and transfer or delete any other projects you own. Resolve ownership of other nonempty organizations as well. Deletion removes General, its content, the personal organization and any empty organizations the account owns alone. Kioku reserves the user slug for 90 days after deletion. Kioku also removes the profile, identities and memberships, revokes sessions and agents, and removes unused invitations the account created. Historical source attribution and audit references remain. Shared project content stays in place.

There is no unauthenticated recovery endpoint or account-merge shortcut. If all linked provider accounts are lost, recovery needs an operator to verify ownership outside the service before a separate administrative migration. No endpoint permits account recovery based solely on an unverified email address.

## Project source authorization policy

The user's Claude fetches each source with the user's own connectors or CLI. The client sends the text and a source_url to Kioku. Human sessions, write agents and MCP clients can submit text. Kioku stores the text as an inbox item and shares the published source pages with project members.

source_url is client-asserted provenance. The server validates the URL format but does not verify that the text came from that URL or that the submitter can read the original. A member can submit different text under an existing source_url. A later submission replaces an earlier waiting item with the same exact URL, even if the later text is older.

Kioku holds no source-access credentials and never retrieves source text. GitHub and Google OAuth authorize login only. Login credentials remain available for identity verification during account administration. Runners receive the submitted text and use the remove flag to process submitted bodies and removal requests. Managed attempts exchange input and output files through provider APIs.
