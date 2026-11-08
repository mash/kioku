package execrunner

import (
	"bytes"
	"encoding/json"
	"time"
)

type input struct {
	Version      int       `json:"version"`
	JobID        string    `json:"job_id"`
	AttemptID    string    `json:"attempt_id"`
	InputURL     string    `json:"input_url"`
	OutputURL    string    `json:"output_url"`
	JobToken     string    `json:"job_token"`
	Model        string    `json:"model"`
	FirstMessage string    `json:"first_message"`
	Prompt       string    `json:"prompt"`
	BudgetCents  int64     `json:"budget_cents"`
	Deadline     time.Time `json:"deadline"`
}
type result struct {
	Version   int    `json:"version"`
	Reason    string `json:"reason"`
	CostCents *int64 `json:"cost_cents"`
}

func parseResult(data []byte) (result, bool) {
	data = bytes.TrimSpace(data)
	var out result
	err := json.Unmarshal(data, &out)
	return out, err == nil && out.Version == 1 && out.CostCents != nil && *out.CostCents >= 0 && *out.CostCents <= 1_000_000
}

const outputLimit = 1 << 20

type byteCounter struct {
	total int64
}

func (b *byteCounter) Write(p []byte) (int, error) {
	b.total += int64(len(p))
	return len(p), nil
}

type tailBuffer struct {
	current, last                    []byte
	nonempty, overflow, lastOverflow bool
	total                            int64
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	b.total += int64(n)
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		part := p
		if end >= 0 {
			part = p[:end]
		}
		b.nonempty = b.nonempty || len(bytes.TrimSpace(part)) > 0
		remaining := outputLimit - len(b.current)
		if len(part) > remaining {
			b.overflow = true
			part = part[:remaining]
		}
		b.current = append(b.current, part...)
		if end < 0 {
			break
		}
		if !b.overflow {
			b.nonempty = len(bytes.TrimSpace(b.current)) > 0
		}
		if b.nonempty {
			b.last, b.current = b.current, b.last
			b.lastOverflow = b.overflow
		}
		b.current = b.current[:0]
		b.nonempty, b.overflow = false, false
		p = p[end+1:]
	}
	return n, nil
}

func (b *tailBuffer) line() []byte {
	if b.nonempty {
		if b.overflow {
			return nil
		}
		return b.current
	}
	if b.lastOverflow {
		return nil
	}
	return b.last
}
