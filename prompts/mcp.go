package prompts

import (
	"maps"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

type MCP struct {
	Instructions string
	Tools        map[string]MCPTool
}
type MCPTool struct {
	Description string
	Properties  map[string]MCPProperty
}
type MCPProperty struct {
	Description string
}

func (r *Rendered) Instructions() string { return r.mcp.Instructions }
func (r *Rendered) MCP() MCP {
	m := r.mcp
	m.Tools = maps.Clone(m.Tools)
	for name, tool := range m.Tools {
		tool.Properties = maps.Clone(tool.Properties)
		m.Tools[name] = tool
	}
	return m
}

func (r *Rendered) loadMCP(body string, known map[string][]string) error {
	root, err := readYAML(MCPFile, body, 1)
	if err != nil {
		return err
	}
	entries, err := mapping(MCPFile, root, 0)
	if err != nil {
		return err
	}
	for key, node := range entries {
		switch key {
		case "instructions":
			r.mcp.Instructions, err = r.text(node)
		case "tools":
			var tools map[string]*yaml.Node
			tools, err = mapping(MCPFile, node, 0)
			if err != nil {
				return err
			}
			for name, node := range tools {
				properties, ok := known[name]
				if name == "" || strings.Contains(name, ".") || known != nil && !ok {
					return invalid(MCPFile, "unknown key", node.Line)
				}
				tool := r.mcp.Tools[name]
				if tool.Properties == nil {
					tool.Properties = map[string]MCPProperty{}
				}
				fields, err := mapping(MCPFile, node, 0)
				if err != nil {
					return err
				}
				for key, node := range fields {
					switch key {
					case "description":
						tool.Description, err = r.description(node)
					case "properties":
						err = r.loadProperties(node, tool.Properties, properties, known != nil)
					default:
						return invalid(MCPFile, "unknown key", node.Line)
					}
					if err != nil {
						return err
					}
				}
				r.mcp.Tools[name] = tool
			}
		default:
			return invalid(MCPFile, "unknown key", node.Line)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *Rendered) loadProperties(node *yaml.Node, properties map[string]MCPProperty, known []string, checkNames bool) error {
	entries, err := mapping(MCPFile, node, 0)
	if err != nil {
		return err
	}
	for name, node := range entries {
		if name == "" || strings.Contains(name, ".") || checkNames && !slices.Contains(known, name) {
			return invalid(MCPFile, "unknown key", node.Line)
		}
		fields, err := mapping(MCPFile, node, 0)
		if err != nil {
			return err
		}
		property := properties[name]
		for key, node := range fields {
			if key != "description" {
				return invalid(MCPFile, "unknown key", node.Line)
			}
			property.Description, err = r.description(node)
			if err != nil {
				return err
			}
		}
		properties[name] = property
	}
	return nil
}

func (r *Rendered) description(node *yaml.Node) (string, error) {
	text, err := r.text(node)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(text) == "" {
		return "", invalid(MCPFile, "empty description", node.Line)
	}
	return text, nil
}
