package cli

const help = `kioku — shared project context

Global flags (before command): -C DIRECTORY --server ORIGIN

  version                             Show client build information
  agent login CODE | list|revoke      Manage project-scoped agent credentials
  account status|sessions|revoke-session|revoke-all|unlink|delete
  login [--provider google|github]      Browser login
  logout | whoami
  org create SLUG | org list
  project create ORG/PROJECT | project join INVITE_URL        Create or join a project
  project list                                List participating projects
  project share list
  project share invite [EMAIL]                 Single-use invite; expires in 7 days
  project share transfer --yes EMAIL_OR_ID     Transfer project ownership
  org transfer --yes ORG EMAIL_OR_ID    Transfer organization ownership
  project share remove EMAIL_OR_ID
  delete SOURCE_URL...                Remove sources at the next generation
  put [FILES...] Save sources, or changed inbox files
  generate [--wait]         Generate wiki from waiting inbox items (wait also pulls)
  pull [ORG/PROJECT]          Bind cwd on first pull; fetch one project
  status | open
  project usage | project audit
  project maintenance                          Collect unreferenced project objects
  project delete --yes                         Delete project and cloud objects
  job status|wait JOB_ID | job retry [--wait] JOB_ID

put accepts one inbox item per request. Projects generate periodically.
Use generate --wait to generate immediately and pull the completed wiki.
All project members can read submitted sources and generated content.
The cwd .kioku/config selects server and namespace; no parent directory search.
First authentication to a non-default server requires approval in a terminal.
Default server: https://kioku.magicians.ee
wiki/, maps/ and inbox/ live in cwd; transfer metadata lives in .kioku/.
Credentials are stored per server in ~/.config/kioku/auth.
Connector agent credentials are stored per project in ~/.config/kioku/agents.
`
