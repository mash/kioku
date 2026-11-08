# Connectors

- `github` and `google` own OAuth endpoints, scopes, authorization options, and identity verification.
- `connector.Connector` has three methods: `OAuth`, `LoginIdentity`, and `Identity`.
- `registry` registers implementations by name. Names are registration data, not interface methods.
- `auth` handles OAuth exchange/refresh and HTTP transport.

`OAuth` returns endpoints, scopes, and authorization options together. Login verification stays separate from account lookup so Google login always validates its ID token.

To add another connector, implement it in its own package and register it in `registry`. Daemon OAuth environment configuration uses the registered names (`<NAME>_CLIENT_ID` and `<NAME>_CLIENT_SECRET`). Add authentication tests and update CLI help and user documentation when exposing a new connector.

GitHub and Google provide Kioku login identities. The client retrieves source text with its own tools and sends the text with source_url. Kioku holds no source-access credentials. source_url is client-asserted provenance; the server does not verify the text against the URL.
