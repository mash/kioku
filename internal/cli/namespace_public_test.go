package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNamespaceSelectionAndCLIOutput(t *testing.T) {
	const internalID = "private-project-id"
	var routes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routes = append(routes, r.URL.Path)
		switch r.URL.Path {
		case "/api/projects":
			json.NewEncoder(w).Encode([]map[string]string{{"namespace": "acme/platform", "name": "Platform"}})
		case "/api/projects/acme/platform/usage":
			w.Write([]byte(`{"project_id":"private-project-id","count":9007199254740993}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	if err := os.Mkdir(filepath.Join(dir, ".kioku"), 0700); err != nil {
		t.Fatal(err)
	}
	initial, _ := json.Marshal(Config{Server: server.URL, Namespace: "acme/platform"})
	if err := os.WriteFile(filepath.Join(dir, ".kioku", "config"), initial, 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, error) {
		var out, stderr bytes.Buffer
		err := Run(context.Background(), append([]string{"-C", dir, "--server", server.URL}, args...), &out, &stderr)
		return out.String(), err
	}
	for _, args := range [][]string{{"project", "list"}, {"project", "usage"}} {
		output, err := run(args...)
		if err != nil {
			t.Fatal(args, err)
		}
		if strings.Contains(output, internalID) || strings.Contains(output, `"project_id"`) {
			t.Fatal("internal ID exposed", output)
		}
		if args[len(args)-1] == "usage" && !strings.Contains(output, "9007199254740993") {
			t.Fatal("numeric precision lost", output)
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, ".kioku", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"project"`) || !strings.Contains(string(b), "acme/platform") {
		t.Fatal(string(b))
	}
	for _, args := range [][]string{{"use", internalID}, {"--namespace", internalID, "usage"}, {"--project", internalID, "usage"}} {
		before := len(routes)
		if _, err := run(args...); err == nil {
			t.Fatal("ID selector accepted", args)
		}
		if len(routes) != before {
			t.Fatal("invalid selector made a request", args)
		}
	}
}

func TestConfigStoresNamespaceOnly(t *testing.T) {
	var config Config
	if err := json.Unmarshal([]byte(`{"server":"https://example.test","token":"test-token","project":"old-id","namespace":"acme/platform"}`), &config); err != nil {
		t.Fatal(err)
	}
	if config.Namespace != "acme/platform" || config.Token != "" {
		t.Fatal("namespace not loaded or internal ID accepted")
	}
	b, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if _, ok := saved["project"]; ok {
		t.Fatal("internal ID persisted")
	}
	if saved["namespace"] != "acme/platform" || saved["token"] != nil {
		t.Fatal("configuration fields lost")
	}
}
