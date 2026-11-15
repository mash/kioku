package store

// catalogSchema is the complete v3 catalog. There are deliberately no foreign
// keys: actor and object IDs in history survive deletion of their principals.
const catalogSchema = `
CREATE TABLE organizations(id TEXT NOT NULL PRIMARY KEY,slug TEXT NOT NULL UNIQUE,data TEXT NOT NULL CHECK(json_valid(data) AND json_type(data)='object')) STRICT;
CREATE TABLE organization_members(org_id TEXT NOT NULL,user_id TEXT NOT NULL,role TEXT NOT NULL CHECK(role IN ('owner','member')),PRIMARY KEY(org_id,user_id)) STRICT;
CREATE TABLE users(id TEXT NOT NULL PRIMARY KEY,email TEXT NOT NULL,data TEXT NOT NULL CHECK(json_valid(data) AND json_type(data)='object')) STRICT;
CREATE TABLE user_identities(user_id TEXT NOT NULL,provider TEXT NOT NULL,identity_id TEXT NOT NULL,PRIMARY KEY(user_id,provider),UNIQUE(provider,identity_id)) STRICT;
CREATE TABLE projects(id TEXT NOT NULL PRIMARY KEY,org_id TEXT NOT NULL,slug TEXT NOT NULL,created INTEGER NOT NULL,deleting INTEGER NOT NULL CHECK(deleting IN (0,1)),wiki_revision TEXT,wiki_incomplete INTEGER NOT NULL CHECK(wiki_incomplete IN (0,1)),next_generate_at INTEGER,view_epoch TEXT,data TEXT NOT NULL CHECK(json_valid(data) AND json_type(data)='object'),UNIQUE(org_id,slug)) STRICT;
CREATE TABLE project_members(project_id TEXT NOT NULL,user_id TEXT NOT NULL,role TEXT NOT NULL CHECK(role IN ('owner','member')),PRIMARY KEY(project_id,user_id)) STRICT;
CREATE TABLE sessions(id TEXT NOT NULL PRIMARY KEY,kind TEXT NOT NULL CHECK(kind IN ('web','cli','mcp','agent','cli_login','oauth_code','login_code')),status TEXT NOT NULL CHECK(status IN ('pending','active')),user_id TEXT NOT NULL,project_id TEXT,expires INTEGER NOT NULL,created INTEGER NOT NULL,data TEXT NOT NULL CHECK(json_valid(data) AND json_type(data)='object'),CHECK(kind IN ('cli_login','oauth_code','login_code') OR status='active'),CHECK(kind='cli_login' OR user_id<>'')) STRICT;
CREATE TABLE invites(id TEXT NOT NULL PRIMARY KEY,project_id TEXT NOT NULL,created_by TEXT NOT NULL,expires INTEGER NOT NULL,used_by TEXT,token_hash TEXT NOT NULL,data TEXT NOT NULL CHECK(json_valid(data) AND json_type(data)='object')) STRICT;
CREATE TABLE oauth_clients(client_id TEXT NOT NULL PRIMARY KEY,data TEXT NOT NULL CHECK(json_valid(data) AND json_type(data)='object')) STRICT;
CREATE TABLE inbox(id TEXT NOT NULL PRIMARY KEY,project_id TEXT NOT NULL,source_url TEXT NOT NULL,remove INTEGER NOT NULL CHECK(remove IN (0,1)),created INTEGER NOT NULL,run_id TEXT,data TEXT NOT NULL CHECK(json_valid(data) AND json_type(data)='object')) STRICT;
CREATE TABLE wiki_files(project_id TEXT NOT NULL,path TEXT NOT NULL,hash TEXT NOT NULL CHECK(length(hash)=64 AND hash NOT GLOB '*[^0-9a-f]*'),data TEXT NOT NULL CHECK(json_valid(data) AND json_type(data)='object'),PRIMARY KEY(project_id,path),CHECK(COALESCE(json_type(data,'$.bytes')='integer' AND json_extract(data,'$.bytes')>=0,0))) STRICT;
CREATE TABLE jobs(id TEXT NOT NULL PRIMARY KEY,project_id TEXT NOT NULL,status TEXT NOT NULL CHECK(status IN ('queued','running','completed','failed','skipped')),created INTEGER NOT NULL,kind TEXT NOT NULL,session_id TEXT,input_ready INTEGER NOT NULL CHECK(input_ready IN (0,1)),cleanup_complete INTEGER CHECK(cleanup_complete IN (0,1)),phase TEXT CHECK(phase IN ('preparing','agent_running','output_received','finalizing')),output_hash TEXT CHECK(length(output_hash)=64 AND output_hash NOT GLOB '*[^0-9a-f]*'),generation_snapshot TEXT,child_id TEXT,parent_id TEXT,data TEXT NOT NULL CHECK(json_valid(data) AND json_type(data)='object'),
CHECK(COALESCE((phase IS NULL AND json_type(data,'$.run')='null') OR (phase IS NOT NULL AND json_type(data,'$.run')='object'),0)),
CHECK(COALESCE((generation_snapshot IS NULL AND json_type(data,'$.view_generation')='null') OR (generation_snapshot IS NOT NULL AND json_type(data,'$.view_generation')='object'),0)),
CHECK(COALESCE((parent_id IS NULL AND json_type(data,'$.view_task')='null') OR (parent_id IS NOT NULL AND json_type(data,'$.view_task')='object'),0)),
CHECK((phase IS NULL AND session_id IS NULL AND output_hash IS NULL AND cleanup_complete IS NULL AND input_ready=0) OR (phase IS NOT NULL AND cleanup_complete IS NOT NULL)),
CHECK(child_id IS NULL OR generation_snapshot IS NOT NULL),CHECK(status<>'running' OR phase IS NOT NULL OR generation_snapshot IS NOT NULL)) STRICT;
CREATE TABLE audit(id TEXT NOT NULL PRIMARY KEY,project_id TEXT NOT NULL,created INTEGER NOT NULL,data TEXT NOT NULL CHECK(json_valid(data) AND json_type(data)='object')) STRICT;
CREATE TABLE views(project_id TEXT NOT NULL,id TEXT NOT NULL,slug TEXT NOT NULL,created_at INTEGER,bundle_hash TEXT CHECK(length(bundle_hash)=64 AND bundle_hash NOT GLOB '*[^0-9a-f]*'),view_epoch TEXT,data TEXT NOT NULL CHECK(json_valid(data) AND json_type(data)='object'),PRIMARY KEY(project_id,id),UNIQUE(project_id,slug),
CHECK(slug<>'wiki' OR (created_at IS NULL AND bundle_hash IS NULL AND view_epoch IS NULL)),
CHECK(COALESCE(CASE WHEN slug='wiki' THEN json_type(data,'$.title') IN ('null','text') AND json_type(data,'$.prompt') IN ('null','text') AND json_type(data,'$.status')='null' AND json_type(data,'$.error')='null' AND json_type(data,'$.job_id')='null' AND json_type(data,'$.wiki_snapshot_id')='null' AND json_type(data,'$.generated_at')='null' ELSE json_type(data,'$.title')='text' AND json_type(data,'$.prompt')='text' AND json_type(data,'$.status')='text' END,0))) STRICT;
CREATE TABLE usage(attempt_id TEXT NOT NULL PRIMARY KEY,project_id TEXT NOT NULL,job_id TEXT NOT NULL UNIQUE,recorded_at INTEGER NOT NULL,data TEXT NOT NULL CHECK(json_valid(data) AND json_type(data)='object'),CHECK(attempt_id=job_id)) STRICT;
CREATE TABLE schema_state(id INTEGER NOT NULL PRIMARY KEY CHECK(id=1),data TEXT NOT NULL CHECK(json_valid(data) AND json_type(data)='object'),CHECK(COALESCE(json_type(data,'$.ready') IN ('true','false'),0))) STRICT;
CREATE INDEX sessions_user ON sessions(user_id,kind,expires,id);
CREATE INDEX sessions_expires ON sessions(expires,id);
CREATE INDEX sessions_kind ON sessions(kind,status,id);
CREATE INDEX sessions_project ON sessions(project_id,kind,user_id,id);
CREATE INDEX projects_due ON projects(next_generate_at,id) WHERE next_generate_at IS NOT NULL AND deleting=0;
CREATE INDEX organization_members_user ON organization_members(user_id,role,org_id);
CREATE INDEX project_members_user ON project_members(user_id,role,project_id);
CREATE INDEX invites_token ON invites(token_hash);
CREATE INDEX invites_project ON invites(project_id,used_by,expires,id);
CREATE INDEX invites_creator ON invites(created_by,used_by,id);
CREATE INDEX jobs_queue ON jobs(created,id) WHERE status='queued';
CREATE INDEX jobs_project_status ON jobs(project_id,status,created,id);
CREATE INDEX jobs_cleanup ON jobs(cleanup_complete,status,id) WHERE phase IS NOT NULL;
CREATE INDEX jobs_session ON jobs(session_id,id) WHERE session_id IS NOT NULL;
CREATE INDEX jobs_child ON jobs(child_id,id);
CREATE INDEX jobs_parent ON jobs(parent_id,status,id);
CREATE INDEX jobs_generation ON jobs(status,generation_snapshot,id);
CREATE INDEX views_order ON views(project_id,created_at,id);
CREATE INDEX wiki_hash ON wiki_files(project_id,hash);
CREATE INDEX audit_history ON audit(project_id,created DESC,id DESC);
CREATE INDEX inbox_source ON inbox(project_id,source_url,run_id);
CREATE INDEX inbox_run ON inbox(project_id,run_id,remove,created,id);
CREATE INDEX usage_project ON usage(project_id,recorded_at,attempt_id);
`
