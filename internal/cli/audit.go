package cli

func (a *App) audit() error {
	p, err := a.projectPath()
	if err != nil {
		return err
	}
	var out any
	_, err = a.request("GET", p+"/audit", nil, &out)
	if err != nil {
		return err
	}
	return a.print(out)
}
