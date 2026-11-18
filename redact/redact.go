// Package redact keeps secret values out of what a host prints: every value added, and every
// variable of the process's environment whose name says it holds a secret.
package redact

import (
	"encoding/base64"
	"os"
	"slices"
	"strings"
	"sync"
)

const Mask = "***"

// minLen is the shortest value worth redacting: shorter ones would mask ordinary words.
const minLen = 8

var (
	mu     sync.RWMutex
	values []string
	once   sync.Once
)

// SecretName says whether a variable's name says it holds a secret.
func SecretName(name string) bool {
	name = strings.ToUpper(name)

	for _, suffix := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "_KEY", "CREDENTIALS"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}

	return false
}

func load() {
	once.Do(func() {
		for _, kv := range os.Environ() {
			name, value, _ := strings.Cut(kv, "=")
			if SecretName(name) {
				add(value)
			}
		}
	})
}

// Add records a secret value and the forms it travels in: base64, and an HTTP Basic credential
// with it as the password.
func Add(value string) {
	load()
	add(value)
}

func add(value string) {
	if len(value) < minLen {
		return
	}

	mu.Lock()
	defer mu.Unlock()

	for _, v := range []string{
		value,
		base64.StdEncoding.EncodeToString([]byte(value)),
		base64.StdEncoding.EncodeToString([]byte("x-access-token:" + value)),
	} {
		if !slices.Contains(values, v) {
			values = append(values, v)
		}
	}

	// longest first, so a value inside another is masked as part of it
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
}

func Redact(text string) string {
	load()

	mu.RLock()
	defer mu.RUnlock()

	for _, v := range values {
		text = strings.ReplaceAll(text, v, Mask)
	}

	return text
}
