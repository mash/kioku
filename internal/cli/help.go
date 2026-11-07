package cli

const help = `kioku — shared project context

Global flags (before command): -C DIRECTORY --server ORIGIN

  version                             Show client build information
  agent login CODE | list|revoke      Manage project-scoped agent credentials
  account status|sessions|revoke-session|revoke-all|disconnect|unlink|delete
  login [--provider google|github]      Browser login
  logout | whoami
  connect google|github                Authorize source access
  org create SLUG | org list
  create ORG/PROJECT | join INVITE_URL        Create or join a project
  list                                List participating projects
  share list
  share invite [EMAIL]                 Single-use invite; expires in 7 days
  share transfer --yes EMAIL_OR_ID     Transfer project ownership
  org transfer --yes ORG EMAIL_OR_ID    Transfer organization ownership
  share remove EMAIL_OR_ID
  put [--wait] [URL|FILES...] Save sources, or changed inbox files
  generate [--full] [--wait]         Generate wiki from saved sources (wait also pulls)
  pull [ORG/PROJECT]          Bind cwd on first pull; fetch one project
  status | open
  usage | audit
  maintenance                          Collect unreferenced project objects
  source remove --yes [--wait] SOURCE_ID        Delete source and invalidate generated pages
  delete --yes                         Delete project and cloud objects
  job status|wait JOB_ID | job retry [--wait] JOB_ID

put saves sources; --wait waits for intake only. Changed projects generate periodically.
Use generate --wait to generate immediately and pull the completed wiki.
All project members can read submitted sources and generated content.
The cwd .kioku/config selects server and namespace; no parent directory search.
For login, connect and agent login, non-default servers require --server or KIOKU_SERVER.
Default server: https://kioku.magicians.ee
wiki/, maps/ and inbox/ live in cwd; transfer metadata lives in .kioku/.
Credentials are stored per server in ~/.config/kioku/auth.
Connector agent credentials are stored per project in ~/.config/kioku/agents.
`
