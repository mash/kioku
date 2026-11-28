package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mash/kioku/internal/atomicfile"
	"github.com/mash/kioku/internal/model"
	"github.com/mash/kioku/internal/source"
)

func (a *App) capturePut(args []string) error {
	f := flag.NewFlagSet("capture put", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	captureToken := captureTokenFlag(f)
	src := f.String("source", "", "capture snapshot source")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 1 || *captureToken == "" || *src == "" {
		return errors.New("usage: kioku capture put --capture-token TOKEN --source SOURCE FILE")
	}
	endpoint, err := a.projectPath()
	if err != nil {
		return err
	}
	unlock, err := a.captureSubmissionLock()
	if err != nil {
		return err
	}
	defer unlock()
	dir, snapshot, err := captureSnapshot(*captureToken, a.captureTmp(*captureToken))
	if err != nil {
		return fmt.Errorf("put: %w", err)
	}
	i := resolveSnapshotSource(snapshot, *src)
	if i < 0 {
		return errors.New("put: 取得元は capture の snapshot にない")
	}
	attempt := snapshot[i]
	sourceID := attempt.Source
	queue, err := readQueue(a.queuePath())
	if err != nil {
		return err
	}
	j := snapshotRow(queue, sourceID)
	if j < 0 {
		return errors.New("put: queue に行が無い（delete 済み）")
	}
	if queue[j].State != queueQueued || !sameQueueAttempt(queue[j], attempt) {
		return errors.New("put: queue の行が再登録されたため、この capture では送らない")
	}
	results, err := readCaptureResults(dir)
	if err != nil {
		return err
	}
	if results[sourceID].Outcome != "" {
		return errors.New("put: この capture は取得元の結果を記録済み")
	}
	path := f.Arg(0)
	if !strings.EqualFold(filepath.Ext(path), ".md") {
		return errors.New("put: capture の資料は Markdown (.md) にする")
	}
	b, err := readCaptureSource(dir, path)
	if err != nil {
		return err
	}
	name := filepath.Base(path)
	content := string(b)
	doc, err := source.FileWithProvenance(name, content)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if !strings.HasPrefix(sourceID, toolSourcePrefix) && doc.URL != sourceID {
		return errors.New("put: source_url が capture の HTTPS 取得元と一致しない")
	}
	if ack := results[sourceID].PutURL; ack != "" {
		_, err := fmt.Fprintf(a.Out, "accepted %s\n", ack)
		return err
	}
	if err := a.selectCredential(false); err != nil {
		return err
	}
	input := model.FileInput{Name: name, Content: content}
	code, err := a.request(http.MethodPost, endpoint+"/put", model.PutRequest{File: &input}, nil)
	if err != nil {
		return err
	}
	if code != http.StatusAccepted {
		return fmt.Errorf("put: 受付を確かめられない（HTTP %d）", code)
	}
	results[sourceID] = captureProgress{PutURL: doc.URL}
	receiptErr := writeCaptureResults(dir, results)
	logErr := a.captureNote("accepted " + doc.URL)
	_, err = fmt.Fprintf(a.Out, "accepted %s\n", doc.URL)
	return errors.Join(receiptErr, logErr, err)
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
		if *reason != "" {
			return usage
		}
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
	dir, snapshot, err := captureSnapshot(*token, a.captureTmp(*token))
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
