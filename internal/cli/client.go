package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/mash/kioku/internal/buildinfo"
)

func (a *App) request(method, path string, input, output any) (int, error) {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(a.ctx, method, a.Config.Server+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Kioku-Client-Version", buildinfo.Version)
	if token := a.authToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := a.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return resp.StatusCode, err
		}
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return resp.StatusCode, errors.New(e.Error)
	}
	if output != nil {
		// Wiki pages have no fixed per-page limit. JSON escaping can also make
		// their wire representation larger than the server's archive limit.
		decoder := json.NewDecoder(resp.Body)
		decoder.UseNumber()
		if err = decoder.Decode(output); err != nil {
			return resp.StatusCode, err
		}
	} else if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}

func (a *App) authToken() string {
	switch a.kind {
	case credentialHuman:
		return a.Config.Token
	case credentialAgent:
		return a.AgentToken
	default:
		return ""
	}
}
