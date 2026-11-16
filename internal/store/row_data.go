package store

type OrganizationData struct {
	Name    string `json:"name"`
	Created int64  `json:"created"`
}
type UserData struct {
	Name string `json:"name"`
}
type ProjectData struct {
	Name          string `json:"name"`
	WikiUpdatedAt *int64 `json:"wiki_updated_at"`
}
type InviteData struct {
	Email *string `json:"email"`
}
type OAuthClientData struct {
	ClientName   string   `json:"client_name"`
	Created      int64    `json:"created"`
	RedirectURIs []string `json:"redirect_uris"`
}
type InboxData struct {
	Submitter string `json:"submitter"`
}
type WikiFileData struct {
	Bytes int64 `json:"bytes"`
}
type AuditData struct {
	UserID string `json:"user_id"`
	Action string `json:"action"`
	Detail string `json:"detail"`
}
type JobData struct {
	Run              *RunData         `json:"run"`
	UserID           string           `json:"user_id"`
	AgentID          *string          `json:"agent_id"`
	AgentBudgetCents int64            `json:"agent_budget_cents"`
	Automatic        bool             `json:"automatic"`
	Error            *string          `json:"error"`
	Revision         *string          `json:"revision"`
	WikiStatus       *string          `json:"wiki_status"`
	Unchanged        bool             `json:"unchanged"`
	Views            []ViewResultData `json:"views"`
	ViewGeneration   *GenerationData  `json:"view_generation"`
	ViewTask         *ViewTaskData    `json:"view_task"`
}
type RunData struct {
	UsesProviderFiles bool    `json:"uses_provider_files"`
	UploadStarted     bool    `json:"upload_started"`
	InputFileID       *string `json:"input_file_id"`
	AbortReason       *string `json:"abort_reason"`
	PreparedAt        int64   `json:"prepared_at"`
	Deadline          int64   `json:"deadline"`
	OutputWaitSince   *int64  `json:"output_wait_since"`
	OutputRejection   *string `json:"output_rejection"`
	BudgetCents       int64   `json:"budget_cents"`
}
type ViewResultData struct {
	ViewID string  `json:"view_id"`
	Slug   string  `json:"slug"`
	Status string  `json:"status"`
	Error  *string `json:"error"`
}
type GenerationData struct {
	Pending   bool  `json:"pending"`
	Remaining int64 `json:"remaining"`
	Next      int   `json:"next"`
}
type ViewTaskData struct {
	ViewID string `json:"view_id"`
}
type ViewData struct {
	Title          *string `json:"title"`
	Prompt         *string `json:"prompt"`
	Status         *string `json:"status"`
	Error          *string `json:"error"`
	JobID          *string `json:"job_id"`
	WikiSnapshotID *string `json:"wiki_snapshot_id"`
	GeneratedAt    *int64  `json:"generated_at"`
}

func NullableString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
func ValueString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
