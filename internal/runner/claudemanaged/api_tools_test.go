package claudemanaged

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2"
)

func TestSessionTools(t *testing.T) {
	params := anthropic.BetaSessionNewParams{Agent: anthropic.BetaSessionNewParamsAgentUnion{
		OfBetaManagedAgentsAgentWithOverridess: &anthropic.BetaManagedAgentsAgentWithOverridesParams{ID: "agent", Tools: wikiTools},
	}}
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ Agent struct{ Tools any } }
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal([]byte(`[{"type":"agent_toolset_20260401","default_config":{"permission_policy":{"type":"always_allow"}},"configs":[{"name":"web_fetch","enabled":false},{"name":"web_search","enabled":false}]}]`), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(body.Agent.Tools, want) {
		t.Fatalf("tools = %#v", body.Agent.Tools)
	}
}
