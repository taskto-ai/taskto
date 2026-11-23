package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSSOProvidersRemovesEnvSecretsFromTheEnvironment(t *testing.T) {
	const name = "TASKTO_TEST_OIDC_SECRET"
	t.Setenv(name, "fake-secret-from-env")
	file := filepath.Join(t.TempDir(), "providers.json")
	doc := `{"providers": [{"id": "idp", "name": "IdP", "issuer": "https://idp.example.com", "client_id": "c",
		"client_secret": {"env": "` + name + `"}}]}`
	if err := os.WriteFile(file, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	providers, names, err := loadSSOProviders(file, os.Getenv, os.Unsetenv)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers) != 1 || providers[0].ClientSecret != "fake-secret-from-env" {
		t.Fatalf("the secret wasn't read: %+v", providers)
	}
	if got, ok := os.LookupEnv(name); ok {
		t.Fatalf("%s is still in the server's environment (%q): the sidecar would inherit it", name, got)
	}
	if len(names) != 1 || names[0] != name {
		t.Fatalf("names = %v", names)
	}
}

func TestLoadSSOProvidersWithoutAFile(t *testing.T) {
	providers, names, err := loadSSOProviders("", os.Getenv, os.Unsetenv)
	if err != nil || providers != nil || names != nil {
		t.Fatalf("%v %v %v", providers, names, err)
	}
}
