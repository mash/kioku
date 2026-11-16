package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"strings"

	"github.com/mash/kioku/internal/blob"
)

// ContentHash is the lowercase SHA-256 of an immutable object's bytes.
type ContentHash string

func HashContent(body []byte) ContentHash {
	sum := sha256.Sum256(body)
	return ContentHash(hex.EncodeToString(sum[:]))
}
func ParseContentHash(s string) (ContentHash, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != sha256.Size || strings.ToLower(s) != s {
		return "", errors.New("invalid content hash")
	}
	return ContentHash(s), nil
}
func WikiContentKey(project string, hash ContentHash) blob.Key {
	return blob.Key("projects/" + project + "/.kioku/wiki/" + string(hash) + ".md")
}
func ViewContentKey(project, view string, hash ContentHash) blob.Key {
	return blob.Key("projects/" + project + "/.kioku/views/" + view + "/bundle-" + string(hash) + ".zip")
}

// UsageValues preserves unknown measurements as null, including after cleanup.
type UsageValues struct {
	OrgID  string `json:"org_id"`
	UserID string `json:"user_id"`
	UsageMeasurements
}

type UsageMeasurements struct {
	Model                    *string `json:"model"`
	InputTokens              *int64  `json:"input_tokens"`
	OutputTokens             *int64  `json:"output_tokens"`
	CacheReadInputTokens     *int64  `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64  `json:"cache_creation_input_tokens"`
	CostCents                *int64  `json:"cost_cents"`
}

// SumMeasurements requires every sample to be known. Overflow makes only this
// measurement unknown; callers aggregate each field independently.
func SumMeasurements(values ...*int64) *int64 {
	if len(values) == 0 {
		return nil
	}
	var total int64
	for _, v := range values {
		if v == nil || *v < 0 || total > math.MaxInt64-*v {
			return nil
		}
		total += *v
	}
	return &total
}
