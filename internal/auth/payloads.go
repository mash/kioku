package auth

import "log/slog"

// OAuthCookie is sealed with the oauth-cookie context; it is never a DB row.
// Its explicit wire encoder is the only place that serializes the verifier.
type OAuthCookie struct {
	Provider   string  `json:"provider"`
	Verifier   string  `json:"verifier"`
	ReturnPath *string `json:"return_path"`
	CLILogin   *string `json:"cli_login"`
	Expires    int64   `json:"expires"`
}
type JobCredential struct {
	JobID   string `json:"job_id"`
	Expires int64  `json:"expires"`
}

func (OAuthCookie) String() string                 { return "[redacted]" }
func (OAuthCookie) GoString() string               { return "[redacted]" }
func (OAuthCookie) LogValue() slog.Value           { return slog.StringValue("[redacted]") }
func (OAuthCookie) MarshalJSON() ([]byte, error)   { return []byte(`"[redacted]"`), nil }
func (JobCredential) String() string               { return "[redacted]" }
func (JobCredential) GoString() string             { return "[redacted]" }
func (JobCredential) LogValue() slog.Value         { return slog.StringValue("[redacted]") }
func (JobCredential) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
