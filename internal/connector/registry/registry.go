// Package registry is the composition point for supported connectors.
// Add new connectors here without modifying auth dispatch.
package registry

import (
	"github.com/mash/kioku/internal/connector"
	"github.com/mash/kioku/internal/connector/github"
	"github.com/mash/kioku/internal/connector/google"
)

type entry struct {
	name      string
	connector connector.Connector
}

var all = []entry{
	{name: "github", connector: github.Connector{}},
	{name: "google", connector: google.Connector{}},
}

func ByName(name string) (connector.Connector, bool) {
	for _, p := range all {
		if p.name == name {
			return p.connector, true
		}
	}
	return nil, false
}

// Names returns the registered connector names for application configuration.
func Names() []string {
	names := make([]string, len(all))
	for i, p := range all {
		names[i] = p.name
	}
	return names
}
