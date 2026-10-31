package ciyaml

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
)

const example = `
stages: [build, test]

variables:
  CGO_ENABLED: "0"
  RETRIES: 3

.go: &go
  image: golang:1.26
  tags: [linux]

build:
  <<: *go
  stage: build
  script:
    - go build -o bin/ ./...
  artifacts:
    paths: [bin/]
    expire_in: 7d

test:
  <<: *go
  tags: [linux, docker]
  timeout: 10m
  variables:
    GOFLAGS: -count=1
  script: go test ./...
  cache:
    key: go-mod
    paths: [.cache/go]
`

func TestParseExample(t *testing.T) {
	cfg, err := Parse([]byte(example))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := strings.Join(cfg.Stages, ","), "build,test"; got != want {
		t.Errorf("stages = %s, want %s", got, want)
	}
	if len(cfg.Variables) != 2 || cfg.Variables[1] != (Variable{"RETRIES", "3"}) {
		t.Errorf("variables = %v", cfg.Variables)
	}
	if len(cfg.Jobs) != 2 {
		t.Fatalf("got %d jobs, want 2 (the .go template is not a job)", len(cfg.Jobs))
	}

	build := cfg.Jobs[0]
	if build.Name != "build" || build.Stage != "build" || build.StageIndex != 0 {
		t.Errorf("build = %s in %s (%d)", build.Name, build.Stage, build.StageIndex)
	}
	if build.Image != "golang:1.26" || strings.Join(build.Tags, ",") != "linux" {
		t.Errorf("merge key not applied: image %q tags %v", build.Image, build.Tags)
	}
	if build.Timeout != DefaultTimeout {
		t.Errorf("default timeout = %v", build.Timeout)
	}
	if build.Artifacts == nil || build.Artifacts.ExpireIn != 7*24*time.Hour || build.Artifacts.Paths[0] != "bin/" {
		t.Errorf("artifacts = %+v", build.Artifacts)
	}

	test := cfg.Jobs[1]
	if test.StageIndex != 1 || test.Timeout != 10*time.Minute {
		t.Errorf("test stage %d timeout %v", test.StageIndex, test.Timeout)
	}
	if strings.Join(test.Tags, ",") != "linux,docker" {
		t.Errorf("own tags must win over merged ones: %v", test.Tags)
	}
	if len(test.Script) != 1 || test.Script[0] != "go test ./..." {
		t.Errorf("a string script is one command: %q", test.Script)
	}
	if test.Cache == nil || test.Cache.Key != "go-mod" {
		t.Errorf("cache = %+v", test.Cache)
	}
}

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse([]byte("lint:\n  image: alpine\n  script: [\"true\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Stages, ",") != "build,test,deploy" {
		t.Errorf("default stages = %v", cfg.Stages)
	}
	if j := cfg.Jobs[0]; j.Stage != "test" || j.StageIndex != 1 {
		t.Errorf("default stage = %s (%d)", j.Stage, j.StageIndex)
	}
}

// cases are files the parser must reject. schema says whether the JSON
// schema rejects them too; some rules only the parser can check.
var invalidCases = []struct {
	name    string
	yaml    string
	path    string // path of the first error
	line    int    // line of the first error
	message string // part of the first error
	schema  bool
}{
	{"empty", "", "", 0, "empty", false},
	{"not a mapping", "- a\n- b\n", "", 1, "must be a mapping", true},
	{"syntax", "build:\n  image: a\n script: b\n", "", 2, "invalid YAML", false},
	{"no jobs", "stages: [build]\n", "", 1, "no jobs", false},
	{"only templates", ".t:\n  image: a\n", "", 1, "no jobs", false},
	{"missing image", "build:\n  script: make\n", "build", 1, "image is required", true},
	{"missing script", "build:\n  image: alpine\n", "build", 1, "script is required", true},
	{"empty script list", "build:\n  image: a\n  script: []\n", "build.script", 3, "non-empty list", true},
	{"script line is a map", "build:\n  image: a\n  script:\n    - echo: hi\n", "build.script[0]", 4, "quote lines", true},
	{"job is a string", "build: make\n", "build", 1, "must be a mapping", true},
	{"unknown job key", "build:\n  image: a\n  script: b\n  imag: c\n", "build.imag", 4, "unknown key", true},
	{"gitlab job key", "build:\n  image: a\n  script: b\n  rules: []\n", "build.rules", 4, "not supported", true},
	{"gitlab top key", "image: alpine\nbuild:\n  image: a\n  script: b\n", "image", 1, "not supported", true},
	{"undefined stage", "build:\n  stage: compile\n  image: a\n  script: b\n", "build.stage", 2, `stage "compile" is not in stages`, false},
	{"bad job name", "\"my job\":\n  image: a\n  script: b\n", "my job", 1, "job names", true},
	{"duplicate stage", "stages: [a, a]\nj:\n  stage: a\n  image: x\n  script: y\n", "stages[1]", 1, "more than once", true},
	{"empty stages", "stages: []\nj:\n  image: x\n  script: y\n", "stages", 1, "non-empty list", true},
	{"reserved variable", "variables:\n  CI_FOO: x\nj:\n  image: x\n  script: y\n", "variables.CI_FOO", 2, "reserved", true},
	{"bad variable name", "j:\n  image: x\n  script: y\n  variables:\n    1A: x\n", "j.variables.1A", 5, "variable names", true},
	{"null variable", "j:\n  image: x\n  script: y\n  variables:\n    A:\n", "j.variables.A", 5, "single value", true},
	{"bad duration", "j:\n  image: x\n  script: y\n  timeout: 10 minutes\n", "j.timeout", 4, "duration such as", true},
	{"timeout too long", "j:\n  image: x\n  script: y\n  timeout: 2d\n", "j.timeout", 4, "between 10s and 1d", false},
	{"duplicate tag", "j:\n  image: x\n  script: y\n  tags: [a, a]\n", "j.tags[1]", 4, "more than once", true},
	{"bad tag", "j:\n  image: x\n  script: y\n  tags: [\"a b\"]\n", "j.tags[0]", 4, "tags are", true},
	{"image with space", "j:\n  image: golang 1.26\n  script: y\n", "j.image", 2, "image reference", true},
	{"artifacts without paths", "j:\n  image: x\n  script: y\n  artifacts:\n    expire_in: 1d\n", "j.artifacts", 5, "paths is required", true},
	{"artifact escapes", "j:\n  image: x\n  script: y\n  artifacts:\n    paths: [../secret]\n", "j.artifacts.paths[0]", 5, "relative path", true},
	{"absolute artifact", "j:\n  image: x\n  script: y\n  artifacts:\n    paths: [/etc]\n", "j.artifacts.paths[0]", 5, "relative path", true},
	{"duplicate key", "j:\n  image: x\n  image: y\n  script: z\n", "j.image", 3, "more than once", false},
}

func TestParseInvalid(t *testing.T) {
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			var errs Errors
			if !errors.As(err, &errs) || len(errs) == 0 {
				t.Fatalf("Parse = %v, want Errors", err)
			}
			first := errs[0]
			if first.Path != tc.path || first.Line != tc.line || !strings.Contains(first.Message, tc.message) {
				t.Errorf("first error = %q at line %d path %q; want %q at line %d path %q",
					first.Message, first.Line, first.Path, tc.message, tc.line, tc.path)
			}
		})
	}
}

func TestParseReportsEveryError(t *testing.T) {
	_, err := Parse([]byte("a:\n  script: x\nb:\n  image: y\n  timeout: 1y\n"))
	var errs Errors
	if !errors.As(err, &errs) {
		t.Fatal(err)
	}
	if len(errs) != 3 {
		t.Fatalf("got %d errors, want 3 (a.image, b.timeout, b.script): %v", len(errs), errs)
	}
	for i := 1; i < len(errs); i++ {
		if errs[i].Line < errs[i-1].Line {
			t.Errorf("errors not in file order: %v", errs)
		}
	}
}

func TestParseTooLarge(t *testing.T) {
	_, err := Parse(bytes.Repeat([]byte("#"), MaxFileSize+1))
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("Parse = %v", err)
	}
}

func TestParseMergeBomb(t *testing.T) {
	// Each template merges the one before it ten times, so walking the
	// last one naively would visit 10^8 mappings.
	var b strings.Builder
	b.WriteString(".t0: &t0 {k: v}\n")
	for i := 1; i <= 8; i++ {
		fmt.Fprintf(&b, ".t%d: &t%d\n  <<: [", i, i)
		for j := 0; j < 10; j++ {
			if j > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "*t%d", i-1)
		}
		b.WriteString("]\n")
	}
	b.WriteString("j:\n  <<: *t8\n  image: x\n  script: y\n")
	_, err := Parse([]byte(b.String()))
	if err == nil || !strings.Contains(err.Error(), "too complex") {
		t.Fatalf("Parse = %v, want the node budget error", err)
	}
}

func TestDuration(t *testing.T) {
	for _, tc := range []struct {
		in, canonical string
		want          time.Duration
	}{
		{"90s", "1m30s", 90 * time.Second},
		{"10m", "10m", 10 * time.Minute},
		{"1h30m", "1h30m", 90 * time.Minute},
		{"7d", "7d", 7 * 24 * time.Hour},
		{"1d2h3m4s", "1d2h3m4s", 26*time.Hour + 3*time.Minute + 4*time.Second},
	} {
		got, err := ParseDuration(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
		if back := FormatDuration(got); back != tc.canonical {
			t.Errorf("FormatDuration(%v) = %q, want %q", got, back, tc.canonical)
		}
	}
	for _, in := range []string{"", "1", "m", "1h 30m", "30m1h", "-1s", "1.5h", "99999999999d"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) succeeded", in)
		}
	}
}

// TestSchema checks that the JSON schema agrees with the parser: it
// accepts the example and rejects the invalid cases it can express.
func TestSchema(t *testing.T) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(Schema))
	if err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("git2-ci.schema.json", doc); err != nil {
		t.Fatal(err)
	}
	schema, err := c.Compile("git2-ci.schema.json")
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}

	validate := func(src string) error {
		var v any
		if err := yaml.Unmarshal([]byte(src), &v); err != nil {
			return err
		}
		// Through JSON, so the instance has the types the validator expects.
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
		if err != nil {
			return err
		}
		return schema.Validate(inst)
	}

	if err := validate(example); err != nil {
		t.Errorf("schema rejects the example: %v", err)
	}
	for _, tc := range invalidCases {
		if !tc.schema {
			continue
		}
		if err := validate(tc.yaml); err == nil {
			t.Errorf("%s: schema accepts a file the parser rejects", tc.name)
		}
	}
}
