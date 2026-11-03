package cli

func (a *App) usage() error {
	p, err := a.projectPath()
	if err != nil {
		return err
	}
	var out any
	_, err = a.request("GET", p+"/usage", nil, &out)
	if err != nil {
		return err
	}
	return a.print(out)
}
