// Package config loads daemon configuration from flags and environment. The
// trust boundary is the loopback interface (§9): the default bind is
// 127.0.0.1, and a bearer token is optional (recommended for non-loopback binds).
package config

import (
	"flag"
	"io"
	"time"
)

type Config struct {
	Addr                 string
	DBPath               string
	BearerToken          string
	ShutdownTimeout      time.Duration
	ScratchTTL           time.Duration
	ScratchSweepInterval time.Duration
	ShutdownDrain        time.Duration
	LogFormat            string
	LogLevel             string
}

// Parse builds a Config from args (nil = none) and an env lookup (e.g. os.Getenv).
// Env supplies secrets (the bearer token) so they don't appear in process args.
func Parse(args []string, env func(string) string) Config {
	fs := flag.NewFlagSet("magisterd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var c Config
	fs.StringVar(&c.Addr, "addr", "127.0.0.1:8080", "listen address (loopback by default)")
	fs.StringVar(&c.DBPath, "db", "magister.db", "SQLite database path")
	fs.DurationVar(&c.ShutdownTimeout, "shutdown-timeout", 10*time.Second, "graceful shutdown deadline")
	fs.DurationVar(&c.ScratchTTL, "scratch-ttl", 24*time.Hour, "reclaim a terminal run's scratch this long after it finishes (0 disables)")
	fs.DurationVar(&c.ScratchSweepInterval, "scratch-sweep-interval", time.Hour, "how often the scratch janitor sweeps")
	fs.DurationVar(&c.ShutdownDrain, "shutdown-drain", 0, "after shutdown begins, keep serving (readyz=503) this long so load balancers drain before accept stops (0 disables)")
	fs.StringVar(&c.LogFormat, "log-format", "text", "log output format: text or json")
	fs.StringVar(&c.LogLevel, "log-level", "info", "log level: debug, info, warn, or error")
	_ = fs.Parse(args)

	c.BearerToken = env("MAGISTER_BEARER_TOKEN")
	if v := env("MAGISTER_ADDR"); v != "" && !flagSet(fs, "addr") {
		c.Addr = v
	}
	if v := env("MAGISTER_DB"); v != "" && !flagSet(fs, "db") {
		c.DBPath = v
	}
	if v := env("MAGISTER_SCRATCH_TTL"); v != "" && !flagSet(fs, "scratch-ttl") {
		if d, err := time.ParseDuration(v); err == nil {
			c.ScratchTTL = d
		}
	}
	if v := env("MAGISTER_SHUTDOWN_DRAIN"); v != "" && !flagSet(fs, "shutdown-drain") {
		if d, err := time.ParseDuration(v); err == nil {
			c.ShutdownDrain = d
		}
	}
	if v := env("MAGISTER_LOG_FORMAT"); v != "" && !flagSet(fs, "log-format") {
		c.LogFormat = v
	}
	if v := env("MAGISTER_LOG_LEVEL"); v != "" && !flagSet(fs, "log-level") {
		c.LogLevel = v
	}
	return c
}

func flagSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
