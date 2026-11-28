package cli

import (
	"bytes"
	"errors"
	"fmt"
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
	maxCaptureAttempts = 2
)

type queueRow struct {
	Date     string `json:"date"`
	Source   string `json:"source"`
	Attempts int    `json:"attempts"`
	State    string `json:"state"`
	Reason   string `json:"reason"`
}

var errQueueFormat = errors.New("invalid queue row: delete .kioku/queue.tsv to discard the old or invalid queue")

func parseQueue(data []byte, validate bool) ([]queueRow, error) {
	var rows = []queueRow{}
	if len(data) == 0 {
		return rows, nil
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(data), "\n"), "\n") {
		row, err := parseQueueRow(line, validate)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func parseQueueRow(line string, validate bool) (queueRow, error) {
	fields := strings.Split(line, "\t")
	if len(fields) != 5 {
		return queueRow{}, errQueueFormat
	}
	attempts, err := strconv.Atoi(fields[2])
	if err != nil || attempts < 0 || attempts > maxCaptureAttempts || (fields[3] != queueQueued && fields[3] != queueFailed) {
		return queueRow{}, errQueueFormat
	}
	if validate {
		if _, err := time.Parse(time.DateOnly, fields[0]); err != nil {
			return queueRow{}, errQueueFormat
		}
		if _, err := parseCaptureSource(fields[1]); err != nil {
			return queueRow{}, errQueueFormat
		}
	}
	return queueRow{fields[0], fields[1], attempts, fields[3], fields[4]}, nil
}

func formatQueue(rows []queueRow) []byte {
	var b strings.Builder
	for _, row := range rows {
		fmt.Fprintf(&b, "%s\t%s\t%d\t%s\t%s\n", row.Date, row.Source, row.Attempts, row.State, row.Reason)
	}
	return []byte(b.String())
}

func (a *App) queuePath() string { return filepath.Join(a.metadataDir(), "queue.tsv") }

func readQueue(path string, validate bool) ([]queueRow, error) {
	data, err := readRegular(path)
	if os.IsNotExist(err) {
		return []queueRow{}, nil
	}
	if err != nil {
		return nil, err
	}
	return parseQueue(data, validate)
}

// updateQueue runs under queueLock. Returning nil leaves the file unchanged.
func (a *App) updateQueue(change func([]queueRow) []queueRow) error {
	return updateLockedFile(a.queuePath(), func(data []byte) ([]byte, bool, error) {
		rows, err := parseQueue(data, false)
		if err != nil {
			return nil, false, err
		}
		updated := change(rows)
		if updated == nil {
			return nil, false, nil
		}
		next := formatQueue(updated)
		return next, !bytes.Equal(data, next), nil
	})
}

func queueText(raw string) string {
	return strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(raw)
}

func removeQueueSource(rows []queueRow, source string) []queueRow {
	return slices.DeleteFunc(rows, func(row queueRow) bool { return row.Source == source })
}

// registerQueueSource takes the validated identity returned by toolHookSource.
func (a *App) registerQueueSource(source string, now time.Time) error {
	if len(source) > maxCaptureSourceBytes {
		return errors.New("capture source exceeds 4,096 bytes")
	}
	if _, err := a.projectPath(); err != nil {
		return err
	}
	unlock, err := a.queueLock()
	if err != nil {
		return err
	}
	defer unlock()
	return a.updateQueue(func(rows []queueRow) []queueRow {
		for _, row := range rows {
			if row.Source == source && row.State == queueQueued {
				return nil
			}
		}
		return append(removeQueueSource(rows, source), queueRow{
			Date: now.Local().Format(time.DateOnly), Source: source, State: queueQueued,
		})
	})
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
	return a.Source == b.Source && a.Date == b.Date && a.Attempts == b.Attempts
}

func snapshotRow(snapshot []queueRow, source string) int {
	return slices.IndexFunc(snapshot, func(row queueRow) bool { return row.Source == source })
}
