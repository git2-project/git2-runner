package config

import (
	"crypto/rand"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
)

func lookup(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

func TestParse(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	encoded := base64.StdEncoding.EncodeToString(key)

	cfg, err := parse(lookup(map[string]string{
		EnvPostgresDSN: "postgres://x", EnvSecretsKey: encoded, EnvRedisAddr: "redis:6379",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GRPCAddr != ":9090" || cfg.RunnerAddr != ":9091" || cfg.PostgresMaxConns != 10 || len(cfg.SecretsKey) != 32 || cfg.Dev {
		t.Errorf("defaults: %+v", cfg)
	}

	_, err = parse(lookup(map[string]string{
		EnvSecretsKey:       base64.StdEncoding.EncodeToString(key[:31]),
		EnvPostgresMaxConns: "0",
		EnvRunnerTLSCert:    "/cert.pem",
		EnvDevConfigDir:     "/ci",
		EnvRedisAddr:        "no-port",
	}))
	if err == nil {
		t.Fatal("invalid configuration accepted")
	}
	for _, want := range []string{EnvPostgresDSN, EnvSecretsKey, EnvPostgresMaxConns, EnvRunnerTLSKey, EnvDevConfigDir, EnvRedisAddr} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(key[:31])) {
		t.Error("the error repeats the secrets key")
	}

	// t.TempDir is absolute on every OS; "/ci" is not absolute on Windows.
	dir := t.TempDir()
	cfg, err = parse(lookup(map[string]string{
		EnvPostgresDSN: "postgres://x", EnvSecretsKey: encoded, EnvDev: "true", EnvDevConfigDir: dir + string(filepath.Separator),
	}))
	if err != nil || cfg.DevConfigDir != dir || cfg.RedisAddr != "" {
		t.Errorf("dev config: %+v %v", cfg, err)
	}
}

func TestParseGitSvcAddr(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	base := map[string]string{EnvPostgresDSN: "postgres://x", EnvSecretsKey: base64.StdEncoding.EncodeToString(key)}
	with := func(kv ...string) map[string]string {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			env[kv[i]] = kv[i+1]
		}
		return env
	}

	cfg, err := parse(lookup(with(EnvGitSvcAddr, "git-svc:9090")))
	if err != nil || cfg.GitSvcAddr != "git-svc:9090" {
		t.Errorf("git-svc address: %+v %v", cfg, err)
	}
	if cfg, _ := parse(lookup(with())); cfg.GitSvcAddr != "" {
		t.Errorf("unset address became %q", cfg.GitSvcAddr)
	}
	if _, err := parse(lookup(with(EnvGitSvcAddr, "git-svc"))); err == nil || !strings.Contains(err.Error(), EnvGitSvcAddr) {
		t.Errorf("address without a port: %v", err)
	}
	_, err = parse(lookup(with(EnvGitSvcAddr, "git-svc:9090", EnvDev, "true", EnvDevConfigDir, t.TempDir())))
	if err == nil || !strings.Contains(err.Error(), EnvGitSvcAddr) || !strings.Contains(err.Error(), EnvDevConfigDir) {
		t.Errorf("both sources set: %v", err)
	}
}
