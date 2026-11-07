# Connectors

- `github` and `google` own OAuth endpoints, scopes, authorization options, and identity verification.
- `connector.Connector` has three methods: `OAuth`, `LoginIdentity`, and `Identity`.
- `registry` registers implementations by name. Names are registration data, not interface methods.
- `auth` handles OAuth exchange/refresh and HTTP transport.

`OAuth` returns endpoints, scopes, and authorization options together. Login verification stays separate from account lookup so Google login always validates its ID token.

To add another connector, implement it in its own package and register it in `registry`. Daemon OAuth environment configuration uses the registered names (`<NAME>_CLIENT_ID` and `<NAME>_CLIENT_SECRET`). Add authentication tests and update CLI help and user documentation when exposing a new connector.

Notion is not implemented here. Do not assume that a Notion connection is also a Kioku login identity. The current interface captures the existing GitHub and Google behavior.

Source retrieval belongs to the current runner/inbox flow. This refactor does not restore the legacy source fetcher removed from main.
