package cli

func (a *App) shareList(args []string, p string) error {
	var v any
	_, err := a.request("GET", p+"/members", nil, &v)
	if err != nil {
		return err
	}
	return a.print(v)
}
