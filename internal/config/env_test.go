package config

import (
	"os"
	"testing"
)

func TestEnv(t *testing.T) {
	t.Setenv("PROJ_METRICS_TOKEN", "obsolete-secret")
	t.Setenv("KIOKU_METRICS_TOKEN", "")
	if err := os.Unsetenv("KIOKU_METRICS_TOKEN"); err != nil {
		t.Fatal(err)
	}
	if got := Env("KIOKU_METRICS_TOKEN", "fallback"); got != "fallback" {
		t.Fatal("unset setting must not read obsolete names")
	}
	t.Setenv("KIOKU_METRICS_TOKEN", "")
	if got := Env("KIOKU_METRICS_TOKEN", "fallback"); got != "fallback" {
		t.Fatal("empty setting must use fallback without reading obsolete names")
	}
	t.Setenv("KIOKU_METRICS_TOKEN", "current-secret")
	if got := Env("KIOKU_METRICS_TOKEN", "fallback"); got != "current-secret" {
		t.Fatal("current setting not recognized")
	}
}
