package cli

func (a *App) orgList(args []string) error {
	var out any
	if _, err := a.request("GET", "/api/orgs", nil, &out); err != nil {
		return err
	}
	return a.print(out)
}
