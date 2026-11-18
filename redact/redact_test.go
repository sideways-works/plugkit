package redact

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestRedactMasksEveryForm(t *testing.T) {
	t.Setenv("SOME_TOKEN", "tok-from-the-environment")
	load()
	Add("tok-from-the-environment") // the environment is read once per process

	tok := "tok-from-the-environment"
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))

	text := "fatal: Authorization: Basic " + basic + " and " + tok
	got := Redact(text)

	if strings.Contains(got, tok) || strings.Contains(got, basic) || strings.Count(got, Mask) != 2 {
		t.Fatalf("redacted: %q", got)
	}

	if Redact("short") != "short" {
		t.Fatal("redacted an ordinary word")
	}
}

func TestSecretName(t *testing.T) {
	for name, want := range map[string]bool{
		"GITHUB_TOKEN": true, "npm_token": true, "DB_PASSWORD": true, "SSH_KEY": true, "AWS_CREDENTIALS": true,
		"KEYBOARD": false, "PATH": false, "MONKEY": false,
	} {
		if got := SecretName(name); got != want {
			t.Errorf("%s: %v", name, got)
		}
	}
}
