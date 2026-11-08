package claudemanaged

import (
	"errors"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2"
)

func TestAPIErrorRetainsTypeWithoutMessage(t *testing.T) {
	provider := &anthropic.Error{StatusCode: 400}
	if err := provider.UnmarshalJSON([]byte(`{"error":{"message":"private-secret","type":"invalid_request_error"}}`)); err != nil {
		t.Fatal(err)
	}
	got := apiError(provider)
	var api *APIError
	if !errors.As(got, &api) || api.Status != 400 || api.Type != "invalid_request_error" {
		t.Fatal(got)
	}
	if got.Error() != "managed agents: HTTP 400" || strings.Contains(got.Error(), "private-secret") {
		t.Fatal(got)
	}
}
