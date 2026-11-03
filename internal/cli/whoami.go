package cli

func (a *App) whoami() error {
	var v any
	if _, err := a.request("GET", "/api/me", nil, &v); err != nil {
		return err
	}
	return a.print(v)
}
