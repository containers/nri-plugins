package main

import (
	"testing"

	tomlv2 "github.com/pelletier/go-toml/v2"
)

func TestUpdateContainerdConfigWritesTimeoutsUnderNRIPlugin(t *testing.T) {
	config := mustUnmarshalTOML(t, `
version = 2
root = "/var/lib/containerd"

[plugins."io.containerd.nri.v1.nri"]
  disable = true
  plugin_registration_timeout = "5s"
  plugin_request_timeout = "2s"
  socket_path = "/var/run/nri/nri.sock"
`)

	updated := updateContainerdConfig(config, &nriConfig{
		registrationTimeout: "25s",
		requestTimeout:      "20s",
	})

	if _, ok := updated[pluginRegistrationTimeoutKey]; ok {
		t.Fatalf("registration timeout written at config root")
	}
	if _, ok := updated[pluginRequestTimeoutKey]; ok {
		t.Fatalf("request timeout written at config root")
	}

	nri := nriPluginSection(t, updated)
	if got, want := nri["disable"], false; got != want {
		t.Fatalf("disable: got %v want %v", got, want)
	}
	if got, want := nri[pluginRegistrationTimeoutKey], "25s"; got != want {
		t.Fatalf("registration timeout: got %v want %v", got, want)
	}
	if got, want := nri[pluginRequestTimeoutKey], "20s"; got != want {
		t.Fatalf("request timeout: got %v want %v", got, want)
	}
	if got, want := nri["socket_path"], "/var/run/nri/nri.sock"; got != want {
		t.Fatalf("socket_path: got %v want %v", got, want)
	}
}

func TestUpdateContainerdConfigNoTimeoutsLeavesNRIEnabled(t *testing.T) {
	config := mustUnmarshalTOML(t, `
version = 2
plugin_registration_timeout = "25s"
plugin_request_timeout = "20s"

[plugins."io.containerd.nri.v1.nri"]
  disable = true
  plugin_request_timeout = "2s"
`)

	updated := updateContainerdConfig(config, &nriConfig{})

	if got, want := updated[pluginRegistrationTimeoutKey], "25s"; got != want {
		t.Fatalf("root registration timeout: got %v want %v", got, want)
	}
	if got, want := updated[pluginRequestTimeoutKey], "20s"; got != want {
		t.Fatalf("root request timeout: got %v want %v", got, want)
	}

	nri := nriPluginSection(t, updated)
	if got, want := nri["disable"], false; got != want {
		t.Fatalf("disable: got %v want %v", got, want)
	}
	if got, want := nri[pluginRequestTimeoutKey], "2s"; got != want {
		t.Fatalf("existing request timeout: got %v want %v", got, want)
	}
}

func TestUpdateContainerdConfigCreatesMissingNRISection(t *testing.T) {
	config := mustUnmarshalTOML(t, `
version = 2
`)

	updated := updateContainerdConfig(config, &nriConfig{
		registrationTimeout: "10s",
		requestTimeout:      "7s",
	})

	nri := nriPluginSection(t, updated)
	if got, want := nri[pluginRegistrationTimeoutKey], "10s"; got != want {
		t.Fatalf("registration timeout: got %v want %v", got, want)
	}
	if got, want := nri[pluginRequestTimeoutKey], "7s"; got != want {
		t.Fatalf("request timeout: got %v want %v", got, want)
	}
}

func mustUnmarshalTOML(t *testing.T, raw string) map[string]any {
	t.Helper()
	var config map[string]any
	if err := tomlv2.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if config == nil {
		t.Fatal("empty toml map")
	}
	return config
}

func nriPluginSection(t *testing.T, config map[string]any) map[string]any {
	t.Helper()
	plugins, ok := config["plugins"].(map[string]any)
	if !ok {
		t.Fatalf("plugins type %T", config["plugins"])
	}
	nri, ok := plugins[nriPluginKey].(map[string]any)
	if !ok {
		t.Fatalf("nri plugin type %T", plugins[nriPluginKey])
	}
	return nri
}
