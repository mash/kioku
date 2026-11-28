package cli

import (
	"bytes"
	"encoding/json"
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

	"github.com/mash/kioku/internal/atomicfile"
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

func parseQueue(data []byte) ([]queueRow, error) {
	var rows = []queueRow{}
	if len(data) == 0 {
		return rows, nil
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(data), "\n"), "\n") {
		row, err := parseQueueRow(line)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func parseQueueRow(line string) (queueRow, error) {
	fields := strings.Split(line, "\t")
	if len(fields) != 5 {
		return queueRow{}, errQueueFormat
	}
	attempts, err := strconv.Atoi(fields[2])
	if err != nil || attempts < 0 || attempts > maxCaptureAttempts || (fields[3] != queueQueued && fields[3] != queueFailed) {
		return queueRow{}, errQueueFormat
	}
	if _, err := time.Parse(time.DateOnly, fields[0]); err != nil {
		return queueRow{}, errQueueFormat
	}
	if _, err := parseCaptureSource(fields[1]); err != nil {
		return queueRow{}, errQueueFormat
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

func removeQueueSource(rows []queueRow, source string) []queueRow {
	return slices.DeleteFunc(rows, func(row queueRow) bool { return row.Source == source })
}

func (a *App) registerQueueSource(source string, now time.Time) error {
	if _, err := parseCaptureSource(source); err != nil {
		return err
	}
	if _, err := a.projectPath(); err != nil {
		return err
	}
	unlock, err := a.localLock()
	if err != nil {
		return err
	}
	defer unlock()
	rows, err := readQueue(a.queuePath())
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Source == source && row.State == queueQueued {
			return nil
		}
	}
	rows = append(removeQueueSource(rows, source), queueRow{
		Date: now.Local().Format(time.DateOnly), Source: source, State: queueQueued,
	})
	return atomicfile.Write(a.queuePath(), formatQueue(rows), 0600)
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

// A normal argument is the exact SOURCE. Generated commands use a compact
// token-bound row reference only when the SOURCE exceeds OS argv limits.
const maxCaptureLiteralSource = 96 << 10

func resolveSnapshotSource(snapshot []queueRow, arg string) int {
	if number, ok := strings.CutPrefix(arg, "@"); ok {
		i, err := strconv.Atoi(number)
		if err == nil && i > 0 && i <= len(snapshot) && strconv.Itoa(i) == number &&
			len(snapshot[i-1].Source) > maxCaptureLiteralSource {
			return i - 1
		}
		return -1
	}
	return snapshotRow(snapshot, arg)
}

func (a *App) captureResult(args []string) error {
	usage := errors.New("usage: kioku capture result --capture-token TOKEN --result accepted|unsaved|skipped [--reason TEXT] SOURCE")
	f := flag.NewFlagSet("capture result", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	token := captureTokenFlag(f)
	result := f.String("result", "", "capture result")
	reason := f.String("reason", "", "reason for the result")
	if err := f.Parse(args); err != nil || f.NArg() != 1 || *token == "" {
		return usage
	}
	switch *result {
	case "accepted":
	case "unsaved", "skipped":
		if strings.TrimSpace(*reason) == "" {
			return usage
		}
	default:
		return usage
	}
	submit, err := a.captureSubmissionLock()
	if err != nil {
		return err
	}
	defer submit()
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
	index := resolveSnapshotSource(snapshot, f.Arg(0))
	if index < 0 {
		return errors.New("capture result: 取得元は capture の snapshot にない")
	}
	src := snapshot[index].Source
	results, err := readCaptureResults(dir)
	if err != nil {
		return err
	}
	progress := results[src]
	if *result == "accepted" && progress.PutURL == "" {
		return errors.New("capture result: HTTP 202 has not been recorded for this source")
	}
	var logErr error
	if progress.Outcome == "" {
		reasonText := queueText(strings.TrimSpace(*reason))
		err := a.updateQueue(func(rows []queueRow) []queueRow {
			for i, row := range rows {
				if row.State != queueQueued || !sameQueueAttempt(row, snapshot[index]) {
					continue
				}
				if *result != "unsaved" {
					return slices.Delete(rows, i, i+1)
				}
				rows[i].Reason = reasonText
				if row.Attempts >= maxCaptureAttempts {
					rows[i].State = queueFailed
				}
				break
			}
			return rows
		})
		if err != nil {
			return err
		}
		progress.Outcome = *result
		results[src] = progress
		if err := writeCaptureResults(dir, results); err != nil {
			return err
		}
		if *result != "accepted" {
			logErr = a.captureNote(*result + " " + src + ": " + reasonText)
		}
	}
	for _, row := range snapshot {
		if results[row.Source].Outcome == "" {
			return logErr
		}
	}
	return errors.Join(logErr, a.releaseCapture(*token))
}

type captureProgress struct {
	PutURL  string `json:"put_url,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

// Keep verified HTTP receipts outside the agent-writable capture directory.
func captureProgressPath(dir string) string {
	return filepath.Join(filepath.Dir(captureLockPathFromTmp(dir)), "capture-progress-"+filepath.Base(dir)+".json")
}

func readCaptureResults(dir string) (map[string]captureProgress, error) {
	results := map[string]captureProgress{}
	data, err := readRegular(captureProgressPath(dir))
	if os.IsNotExist(err) {
		return results, nil
	}
	if err != nil {
		return nil, err
	}
	if err := decodeJSON(data, &results); err != nil {
		return nil, err
	}
	if results == nil {
		return nil, errors.New("invalid capture results")
	}
	return results, nil
}

func writeCaptureResults(dir string, results map[string]captureProgress) error {
	data, err := json.Marshal(results)
	if err != nil {
		return err
	}
	return atomicfile.Write(captureProgressPath(dir), data, 0600)
}
