package main

import (
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestConfigNamesEveryMissingVariable(t *testing.T) {
	_, err := loadConfig(envOf(nil))
	require.Error(t, err)
	for _, name := range []string{"PUBLIC_URL", "API_KEY", "WEBHOOK_URL", "WEBHOOK_SECRET", "RETURN_URL", "no provider enabled"} {
		require.Contains(t, err.Error(), name)
	}
}

func TestConfig(t *testing.T) {
	valid := map[string]string{
		"PUBLIC_URL":     "https://pay.example",
		"API_KEY":        strings.Repeat("k", 32),
		"WEBHOOK_URL":    "https://shop.example/hook",
		"WEBHOOK_SECRET": "s",
		"RETURN_URL":     "https://shop.example/done?x=1",
		"MOCK":           "true",
	}
	c, err := loadConfig(envOf(valid))
	require.NoError(t, err)
	require.Equal(t, "checkout.db", c.DBPath)
	require.Equal(t, ":8080", c.ListenAddr)
	require.True(t, c.Mock)

	with := func(k, v string) map[string]string {
		m := maps.Clone(valid)
		m[k] = v
		return m
	}
	for _, tt := range []struct{ key, value, want string }{
		{"API_KEY", "short", "API_KEY must be at least 32 characters"},
		{"PUBLIC_URL", "https://pay.example/", "PUBLIC_URL must not end with a slash"},
		{"RETURN_URL", "/done", "RETURN_URL must be an absolute"},
		{"MOCK", "yes please", "MOCK must be true or false"},
		{"ESEWA_SECRET_KEY", "x", "must be set together"},
	} {
		_, err := loadConfig(envOf(with(tt.key, tt.value)))
		require.ErrorContains(t, err, tt.want, tt.key)
	}
}
