package cli

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/mash/kioku/internal/publicapi"
)

func (a *App) print(v any) error {
	b, err := json.Marshal(publicapi.Value(v))
	if err != nil {
		return err
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	if err = decoder.Decode(&value); err != nil {
		return err
	}
	a.publicProjectNames(value)
	b, err = json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(a.Out, string(b))
	return err
}

// API project identifiers are private implementation details, not CLI selectors.
func (a *App) publicProjectNames(value any) {
	switch v := value.(type) {
	case map[string]any:
		for _, key := range []string{"project_id", "project"} {
			if _, ok := v[key]; ok {
				if a.Config.Namespace != "" {
					v["namespace"] = a.Config.Namespace
				}
				delete(v, key)
			}
		}
		for _, child := range v {
			a.publicProjectNames(child)
		}
	case []any:
		for _, child := range v {
			a.publicProjectNames(child)
		}
	}
}
