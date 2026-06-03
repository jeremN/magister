package config

import (
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	c := Parse(nil, func(string) string { return "" })
	if c.Addr != "127.0.0.1:8080" {
		t.Errorf("default addr = %q, want loopback", c.Addr)
	}
	if c.DBPath != "magister.db" {
		t.Errorf("default db = %q", c.DBPath)
	}
	if c.BearerToken != "" {
		t.Errorf("bearer should be empty by default")
	}
	if c.ShutdownTimeout != 10*time.Second {
		t.Errorf("shutdown timeout = %v", c.ShutdownTimeout)
	}
}

func TestFlagsOverrideDefaults(t *testing.T) {
	c := Parse([]string{"-addr", ":9999", "-db", "/tmp/x.db"}, func(string) string { return "" })
	if c.Addr != ":9999" || c.DBPath != "/tmp/x.db" {
		t.Errorf("flags not applied: %+v", c)
	}
}

func TestEnvSuppliesBearer(t *testing.T) {
	env := func(k string) string {
		if k == "MAGISTER_BEARER_TOKEN" {
			return "secret"
		}
		return ""
	}
	c := Parse(nil, env)
	if c.BearerToken != "secret" {
		t.Errorf("bearer from env not applied: %q", c.BearerToken)
	}
}
