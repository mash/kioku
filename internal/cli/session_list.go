package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const contextLimit = 10000

func kiokuCommand(exe, server string) string {
	exe = shellQuote(exe)
	if server != "" && server != defaultServer {
		exe += " --server " + shellQuote(server)
	}
	return exe
}

func shellQuote(value string) string {
	if strings.ContainsAny(value, " \t\r\n'\"\\$`;&|<>()*?[]{}!#~") {
		value = "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
	}
	return value
}

func templateVars(exe, server, ns, p string) map[string]string {
	return map[string]string{"kioku": kiokuCommand(exe, server), "kioku_default": kiokuCommand(exe, defaultServer), "p_arg": shellQuote(p), "server": server, "namespace": ns, "p": p, "wiki": filepath.Join(p, "wiki") + string(filepath.Separator)}
}

func projectKey(p projectEntry) string { return p.Server + "\x00" + p.Namespace }

func renderProjectList(kioku string, ps []projectEntry, hints map[string][]string, limit int) string {
	var lines []string
	for _, matched := range []bool{true, false} {
		for _, p := range ps {
			hs := hints[projectKey(p)]
			triggerHit := false
			for _, hint := range hs {
				triggerHit = triggerHit || strings.HasPrefix(hint, "trigger 語 ")
			}
			if triggerHit != matched {
				continue
			}
			description := strings.NewReplacer("\r", " ", "\n", " ").Replace(p.Description)
			if p.General {
				description = "General"
			} else if description == "" {
				description = "（説明なし）"
			}
			line := "- " + p.Namespace
			if p.Server != "" && p.Server != defaultServer {
				line += "［server: " + p.Server + "］"
			}
			line += ": " + description
			for _, hint := range hs {
				line += "［ヒント: " + hint + "］"
			}
			if hints == nil && len(p.Triggers) > 0 {
				line += "［trigger 語: " + strings.Join(p.Triggers, "、") + "］"
			}
			lines = append(lines, line)
		}
	}
	result := strings.Join(lines, "\n")
	if limit <= 0 || utf8.RuneCountInString(result) <= limit {
		return result
	}
	for n := len(lines) - 1; n >= 0; n-- {
		tail := fmt.Sprintf("…ほか %d 件。%s session list で全件を見る。", len(lines)-n, kioku)
		result = strings.Join(append(append([]string{}, lines[:n]...), tail), "\n")
		if utf8.RuneCountInString(result) <= limit {
			return result
		}
	}
	return ""
}
