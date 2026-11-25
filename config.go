package main

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Config is read from environment variables only.
type Config struct {
	PublicURL     string // no trailing slash
	APIKey        string
	WebhookURL    string
	WebhookSecret string
	ReturnURL     string
	DBPath        string
	ListenAddr    string

	EsewaSecretKey, EsewaProductCode string
	EsewaSandbox                     bool
	KhaltiSecretKey                  string
	KhaltiSandbox                    bool
	Mock                             bool
}

// loadConfig reads the configuration and reports every problem at once.
func loadConfig(getenv func(string) string) (Config, error) {
	var errs []error
	bad := func(name, why string) { errs = append(errs, fmt.Errorf("%s %s", name, why)) }
	required := func(name string) string {
		v := getenv(name)
		if v == "" {
			bad(name, "is required")
		}
		return v
	}
	absURL := func(name, v string) {
		if u, err := url.Parse(v); v != "" && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "") {
			bad(name, "must be an absolute http(s) URL")
		}
	}
	boolean := func(name string) bool {
		v := getenv(name)
		if v == "" {
			return false
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			bad(name, "must be true or false")
		}
		return b
	}
	orDefault := func(name, def string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return def
	}

	c := Config{
		PublicURL:        required("PUBLIC_URL"),
		APIKey:           required("API_KEY"),
		WebhookURL:       required("WEBHOOK_URL"),
		WebhookSecret:    required("WEBHOOK_SECRET"),
		ReturnURL:        required("RETURN_URL"),
		DBPath:           orDefault("DB_PATH", "checkout.db"),
		ListenAddr:       orDefault("LISTEN_ADDR", ":8080"),
		EsewaSecretKey:   getenv("ESEWA_SECRET_KEY"),
		EsewaProductCode: getenv("ESEWA_PRODUCT_CODE"),
		EsewaSandbox:     boolean("ESEWA_SANDBOX"),
		KhaltiSecretKey:  getenv("KHALTI_SECRET_KEY"),
		KhaltiSandbox:    boolean("KHALTI_SANDBOX"),
		Mock:             boolean("MOCK"),
	}
	absURL("PUBLIC_URL", c.PublicURL)
	if strings.HasSuffix(c.PublicURL, "/") {
		bad("PUBLIC_URL", "must not end with a slash")
	}
	absURL("WEBHOOK_URL", c.WebhookURL)
	absURL("RETURN_URL", c.ReturnURL)
	if c.APIKey != "" && len(c.APIKey) < 32 {
		bad("API_KEY", "must be at least 32 characters")
	}
	if (c.EsewaSecretKey == "") != (c.EsewaProductCode == "") {
		bad("ESEWA_SECRET_KEY and ESEWA_PRODUCT_CODE", "must be set together")
	}
	if c.EsewaSecretKey == "" && c.KhaltiSecretKey == "" && !c.Mock {
		errs = append(errs, errors.New("no provider enabled: set ESEWA_SECRET_KEY and ESEWA_PRODUCT_CODE, KHALTI_SECRET_KEY, or MOCK=true"))
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration:\n%w", err)
	}
	return c, nil
}
