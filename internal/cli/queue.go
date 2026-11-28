package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	queueQueued        = "queued"
	queueFailed        = "failed"
	maxCaptureAttempts = 3
)

type queueRow struct {
	Date     string `json:"date"`
	URL      string `json:"url"`
	Note     string `json:"note"`
	Attempts int    `json:"attempts"`
	State    string `json:"state"`
	Reason   string `json:"reason"`
}

func parseQueue(data []byte) ([]queueRow, error) {
	rows := []queueRow{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 6 {
			return nil, errors.New("invalid queue row")
		}
		attempts, err := strconv.Atoi(fields[3])
		if err != nil || attempts < 0 || attempts > maxCaptureAttempts || (fields[4] != queueQueued && fields[4] != queueFailed) {
			return nil, errors.New("invalid queue row")
		}
		rows = append(rows, queueRow{fields[0], fields[1], fields[2], attempts, fields[4], fields[5]})
	}
	return rows, nil
}

func formatQueue(rows []queueRow) []byte {
	var b strings.Builder
	for _, row := range rows {
		fmt.Fprintf(&b, "%s\t%s\t%s\t%d\t%s\t%s\n", row.Date, row.URL, row.Note, row.Attempts, row.State, row.Reason)
	}
	return []byte(b.String())
}

func (a *App) queuePath() string { return filepath.Join(a.metadataDir(), "queue.tsv") }

func readQueue(path string) ([]queueRow, error) {
	data, err := readRegular(path)
	if os.IsNotExist(err) {
		return []queueRow{}, nil
	}
	if err != nil {
		return nil, err
	}
	return parseQueue(data)
}

// updateQueue runs under localLock, which also orders capture put and delete.
func (a *App) updateQueue(change func([]queueRow) []queueRow) error {
	return updateLockedFile(a.queuePath(), func(data []byte) ([]byte, bool, error) {
		rows, err := parseQueue(data)
		if err != nil {
			return nil, false, err
		}
		next := formatQueue(change(rows))
		return next, !bytes.Equal(data, next), nil
	})
}

func queueText(raw string) string {
	return strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(raw)
}

func removeRows(rows []queueRow, urls map[string]bool) []queueRow {
	return slices.DeleteFunc(rows, func(row queueRow) bool { return urls[row.URL] })
}

func (a *App) addQueueURLs(urls []string, note string, now time.Time) (int, error) {
	if _, err := a.projectPath(); err != nil {
		return 0, err
	}
	unlock, err := a.localLock()
	if err != nil {
		return 0, err
	}
	defer unlock()
	var count int
	err = a.updateQueue(func(rows []queueRow) []queueRow {
		date, note := now.Local().Format(time.DateOnly), queueText(note)
		added := make([]queueRow, 0, len(urls))
		seen := make(map[string]bool, len(urls))
		// Keep the last occurrence, just as successive puts move a URL to the end.
		for i := len(urls) - 1; i >= 0; i-- {
			u := urls[i]
			if !seen[u] {
				seen[u] = true
				added = append(added, queueRow{Date: date, URL: u, Note: note, State: queueQueued})
			}
		}
		slices.Reverse(added)
		rows = append(removeRows(rows, seen), added...)
		count, _ = queueCounts(rows)
		return rows
	})
	return count, err
}

func queueCounts(rows []queueRow) (queued, failed int) {
	for _, row := range rows {
		if row.State == queueQueued {
			queued++
		} else if row.State == queueFailed {
			failed++
		}
	}
	return
}

func sameQueueAttempt(a, b queueRow) bool {
	return a.URL == b.URL && a.Date == b.Date && a.Note == b.Note && a.Attempts == b.Attempts
}

func (a *App) captureResult(args []string) error {
	usage := errors.New("usage: kioku capture result --capture-token TOKEN --result accepted|unsaved|unknown --reason TEXT URL")
	f := flag.NewFlagSet("capture result", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	token := captureTokenFlag(f)
	result := f.String("result", "", "capture result")
	reason := f.String("reason", "", "reason for the result")
	if err := f.Parse(args); err != nil || f.NArg() != 1 || strings.TrimSpace(*reason) == "" {
		return usage
	}
	switch *result {
	case "accepted", "unsaved", "unknown":
	default:
		return usage
	}
	unlock, err := a.localLock()
	if err != nil {
		return err
	}
	defer unlock()
	dir, err := resolveCaptureDir(*token, a.captureTmp(*token))
	if err != nil {
		return err
	}
	snapshot, err := captureSnapshot(dir)
	if err != nil {
		return err
	}
	u := f.Arg(0)
	i := slices.IndexFunc(snapshot, func(row queueRow) bool { return row.URL == u })
	if i == -1 {
		return errors.New("capture result: URL は capture のスナップショットにない")
	}
	reasonText := queueText(*reason)
	err = a.updateQueue(func(rows []queueRow) []queueRow {
		for iRow, row := range rows {
			if !sameQueueAttempt(row, snapshot[i]) {
				continue
			}
			if *result == "accepted" {
				return slices.Delete(rows, iRow, iRow+1)
			}
			rows[iRow].Reason = reasonText
			if row.Attempts >= maxCaptureAttempts {
				rows[iRow].State = queueFailed
			}
			break
		}
		return rows
	})
	return errors.Join(err, a.captureNote(u+": "+reasonText))
}
