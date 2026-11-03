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
