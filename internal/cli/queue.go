package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type queueRow struct{ Date, URL, Note string }

func parseQueue(data []byte) ([]queueRow, error) {
	var rows []queueRow
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			return nil, errors.New("invalid queue row")
		}
		rows = append(rows, queueRow{fields[0], fields[1], fields[2]})
	}
	return rows, nil
}

func formatQueue(rows []queueRow) []byte {
	var b strings.Builder
	for _, row := range rows {
		fmt.Fprintf(&b, "%s\t%s\t%s\n", row.Date, row.URL, row.Note)
	}
	return []byte(b.String())
}

func addRow(rows []queueRow, date, url, note string) []queueRow {
	rows = removeRows(rows, []string{url})
	note = strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(note)
	return append(rows, queueRow{date, url, note})
}

func removeRows(rows []queueRow, urls []string) []queueRow {
	removed := make(map[string]bool, len(urls))
	for _, url := range urls {
		removed[url] = true
	}
	var kept []queueRow
	for _, row := range rows {
		if !removed[row.URL] {
			kept = append(kept, row)
		}
	}
	return kept
}

func (a *App) queue(args []string, now time.Time) error {
	usage := errors.New("usage: kioku queue add URL NOTE | list | remove [--capture-token TOKEN] URL... [--reason TEXT]")
	if len(args) == 0 {
		return usage
	}
	reason := ""
	token := ""
	if args[0] == "remove" && len(args) > 1 && args[1] == "--capture-token" {
		if len(args) < 4 {
			return usage
		}
		token = args[2]
		if !captureTokenPattern.MatchString(token) {
			return usage
		}
		args = append([]string{"remove"}, args[3:]...)
	}
	if args[0] == "remove" {
		for i, arg := range args[1:] {
			if arg == "--reason" {
				if i+3 != len(args) {
					return usage
				}
				reason = args[i+2]
				args = args[:i+1]
				break
			}
		}
	}
	switch args[0] {
	case "add":
		if len(args) != 3 || strings.TrimSpace(args[2]) == "" {
			return usage
		}
		if !strings.HasPrefix(args[1], "https://") {
			return errors.New("queue add: URL must start with https://")
		}
		if strings.ContainsAny(args[1], "\t\r\n") {
			return errors.New("queue add: URL must not contain tabs or newlines")
		}
	case "list":
		if len(args) != 1 {
			return usage
		}
	case "remove":
		if len(args) < 2 {
			return usage
		}
	default:
		return usage
	}
	if _, err := a.projectPath(); err != nil {
		return err
	}
	path := filepath.Join(a.metadataDir(), "queue.tsv")
	if args[0] == "list" {
		data, err := readRegular(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = a.Out.Write(data)
		return err
	}
	unlock, err := a.localLock()
	if err != nil {
		return err
	}
	defer unlock()
	if reason != "" {
		if err := a.captureNote(strings.Join(args[1:], " ") + ": " + reason); err != nil {
			return err
		}
	}
	return updateLockedFile(path, func(data []byte) ([]byte, bool, error) {
		rows, err := parseQueue(data)
		if err != nil {
			return nil, false, err
		}
		if args[0] == "add" {
			rows = addRow(rows, now.Local().Format("2006-01-02"), args[1], args[2])
		} else {
			if token == "" {
				rows = removeRows(rows, args[1:])
			} else {
				dir, err := a.captureTokenDir(token)
				if err != nil {
					return nil, false, err
				}
				snapshot, err := captureSnapshot(dir)
				if err != nil {
					return nil, false, err
				}
				eligible := map[queueRow]bool{}
				for _, row := range snapshot {
					for _, url := range args[1:] {
						if row.URL == url {
							eligible[row] = true
						}
					}
				}
				kept := rows[:0]
				for _, row := range rows {
					if !eligible[row] {
						kept = append(kept, row)
					}
				}
				rows = kept
			}
		}
		next := formatQueue(rows)
		return next, !bytes.Equal(data, next), nil
	})
}
