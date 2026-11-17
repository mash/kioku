package cli

const help = `kioku — shared project context

Global flags (before command): -n ORG/PROJECT; -C DIRECTORY; --server ORIGIN
  -n selects a projects.json path or ~/.local/share/kioku/projects/ORG/PROJECT.
  It creates the directory; use pull first to bind an existing project.
  -C with -n binds the specified directory to that namespace on pull.
  Without -n, commands use cwd (or -C) .kioku/config; create requires -n.
  Command flags must precede positional arguments, except fetch URL --out FILE and queue remove URL... --reason TEXT.
  put and queue remove accept --capture-token TOKEN before files or URLs.
  -n cannot be combined with project join.

  version                             Show client build information
  session select [--none]             Select and read the wiki entry, or decline
  session list                        List local projects and trigger words
  session hook [--event EVENT] --prompts DIR     Handle Claude Code hook JSON on stdin
  agent login CODE | list|revoke      Manage project-scoped agent credentials
  account status|sessions|revoke-session|revoke-all|unlink|delete
  login [--provider google|github]      Browser login
  logout | whoami
  org create SLUG | org list
  project create [--description TEXT]          Create and bind the selected directory
  project join [--description TEXT] INVITE_URL       Join and bind cwd (or -C)
  project triggers [WORD...]                  Replace trigger words
  project list                                List participating projects
  project share list
  project share invite [EMAIL]                 Single-use invite; expires in 7 days
  project share transfer --yes EMAIL_OR_ID     Transfer project ownership
  org transfer --yes ORG EMAIL_OR_ID    Transfer organization ownership
  project share remove EMAIL_OR_ID
  delete SOURCE_URL...                Remove sources at the next generation
  fetch URL --out FILE [--dir DIR]  HTTPS GET (20 MiB max) into the owned capture directory.
  capture run [--min N] | capture unlock --token TOKEN
  queue add URL NOTE | queue list | queue remove [--capture-token TOKEN] URL... [--reason TEXT]
  put [--capture-token TOKEN] FILE...                        Save sources from files or directories
  generate [--wait]         Generate wiki from waiting inbox items (wait also pulls)
  pull [--description TEXT]           Bind on first pull; fetch one project
  status | open
  project usage | project audit
  project delete --yes                         Delete project and cloud objects
  job status|wait JOB_ID

put accepts one inbox item per request. The server generates a project 10 seconds after its last put by default.
Use generate --wait to generate immediately and pull the completed wiki.
All project members can read submitted sources and generated content.
The selected directory's .kioku/config selects server and namespace; no parent search.
Namespace directory selection resolves server from --server, KIOKU_SERVER, then the default.
The CLI maintains ~/.config/kioku/projects.json; --description updates its entry.
A different existing path stays in projects.json; the CLI prints it to stderr.
First authentication to a non-default server requires approval in a terminal.
Default server: https://kioku.magicians.ee
wiki/ lives in the selected directory; transfer metadata lives in .kioku/.
Credentials are stored per server in ~/.config/kioku/auth.
Connector agent credentials are stored per project in ~/.config/kioku/agents.
With CLAUDE_CODE_SESSION_ID, successful pull/create/join remember selection in
${XDG_STATE_HOME:-~/.local/state}/kioku/sessions/SID.json; no source acceptance ledger.
State keeps server, namespace, mode, path, and optional last_pull_at/last_pull_result.
session select --none stores only declined_at and requires CLAUDE_CODE_SESSION_ID.
Hooks write selection only on trigger auto-selection; background pull only updates
the same selection. Failed create/join do not write state. Failed pull
updates the last pull only for the same server, namespace, and path.
session select also records a failed pull when the local copy exists.
State expires 30 days after file mtime; every write refreshes mtime.
`
