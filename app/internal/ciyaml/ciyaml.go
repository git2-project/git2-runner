// Package ciyaml parses and validates .git2-ci.yml, the file that describes
// a repository's pipeline.
//
// The format is spec v0, documented for users in docs/api/ci-yaml.md and as
// a JSON schema in git2-ci.schema.json. It is a small subset of GitLab CI:
// stages, variables, and jobs with image, script, variables, tags, timeout,
// artifacts and cache. YAML anchors, aliases and merge keys work, and
// top-level keys starting with "." are templates that are not jobs.
//
// Parse reports every problem it finds, each with its line and column, so
// a user can fix the file in one go.
package ciyaml

import (
	_ "embed"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// FileName is the name of the CI file at the root of a repository.
const FileName = ".git2-ci.yml"

// Limits and defaults of spec v0.
const (
	MaxFileSize     = 256 << 10
	MaxJobs         = 200
	MaxStages       = 50
	MaxVariables    = 200
	MaxTags         = 20
	MaxPaths        = 50
	MaxValueSize    = 32 << 10
	DefaultStage    = "test"
	DefaultTimeout  = time.Hour
	MinTimeout      = 10 * time.Second
	MaxTimeout      = 24 * time.Hour
	DefaultExpireIn = 30 * 24 * time.Hour
	MinExpireIn     = time.Hour
	MaxExpireIn     = 365 * 24 * time.Hour
)

// maxNodes bounds the work spent on one file. YAML aliases can make a small
// file expand into a huge tree ("billion laughs").
const maxNodes = 100_000

// DefaultStages are the stages of a file without a stages key.
var DefaultStages = []string{"build", "test", "deploy"}

// Schema is the JSON schema of spec v0, for editors and other tools. It
// accepts a subset of what Parse rejects: Parse also checks rules a schema
// cannot express, such as a job's stage being listed in stages.
//
//go:embed git2-ci.schema.json
var Schema []byte

// Config is a valid .git2-ci.yml.
type Config struct {
	// Stages in the order they run.
	Stages []string
	// Variables for every job, in file order.
	Variables []Variable
	// Jobs in file order.
	Jobs []Job
}

// Variable is an environment variable set by the file.
type Variable struct {
	Name  string
	Value string
}

// Job is one job of the file.
type Job struct {
	Name string
	// Stage is the job's stage and StageIndex its position in
	// Config.Stages.
	Stage      string
	StageIndex int
	Image      string
	Script     []string
	// Variables of this job, in file order. They override the file's
	// variables with the same name.
	Variables []Variable
	// Tags must all be labels of the runner that takes the job.
	Tags      []string
	Timeout   time.Duration
	Artifacts *Artifacts
	Cache     *Cache
}

// Artifacts are files a job keeps after it ends.
type Artifacts struct {
	Paths    []string
	ExpireIn time.Duration
}

// Cache is files kept between runs of a job. Accepted in v0, used from
// WBS-10.6.
type Cache struct {
	Key   string
	Paths []string
}

// Error is one problem in a file. Its text never contains anything but the
// file's own content, so it is safe to show to the file's author.
type Error struct {
	// Path is a dotted path such as "build.script[1]", or empty for the
	// whole file.
	Path string
	// Line and Column start at 1; 0 means unknown.
	Line, Column int
	Message      string
}

func (e Error) Error() string {
	var b strings.Builder
	if e.Line > 0 {
		fmt.Fprintf(&b, "line %d: ", e.Line)
	}
	if e.Path != "" {
		b.WriteString(e.Path + ": ")
	}
	b.WriteString(e.Message)
	return b.String()
}

// Errors is every problem found in a file, in file order. Parse returns it
// as its error.
type Errors []Error

func (es Errors) Error() string {
	msgs := make([]string, len(es))
	for i, e := range es {
		msgs[i] = e.Error()
	}
	return strings.Join(msgs, "; ")
}

var (
	// nameRE is the rule for job, stage and runner tag names.
	nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	// tagRE is the rule for job tags, the same as for runner labels.
	tagRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,63}$`)
	// variableRE is the rule for variable names.
	variableRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,99}$`)
	// durationRE accepts "90s", "10m", "1h30m", "7d".
	durationRE = regexp.MustCompile(`^(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$`)
	// yamlErrRE splits yaml's own error messages into line and text.
	yamlErrRE = regexp.MustCompile(`^yaml: line (\d+): (.*)$`)
)

// ReservedPrefixes are variable name prefixes git2 keeps for itself.
var ReservedPrefixes = []string{"CI_", "GIT2_"}

// unsupportedTop are GitLab CI top-level keys that v0 does not have. Naming
// them gives a better message than treating them as job names.
var unsupportedTop = map[string]bool{
	"default": true, "include": true, "workflow": true, "image": true, "services": true,
	"before_script": true, "after_script": true, "cache": true, "pages": true,
}

// unsupportedJob are GitLab CI job keys that v0 does not have.
var unsupportedJob = map[string]bool{
	"rules": true, "only": true, "except": true, "needs": true, "services": true,
	"before_script": true, "after_script": true, "when": true, "allow_failure": true,
	"parallel": true, "extends": true, "environment": true, "retry": true,
	"dependencies": true, "coverage": true, "interruptible": true, "trigger": true,
	"inherit": true, "release": true, "resource_group": true, "secrets": true,
	"id_tokens": true, "hooks": true,
}

// Parse parses and validates a .git2-ci.yml file. On failure the error is
// Errors.
func Parse(content []byte) (*Config, error) {
	if len(content) > MaxFileSize {
		return nil, Errors{{Message: fmt.Sprintf("the file is larger than %d KiB", MaxFileSize>>10)}}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, Errors{yamlError(err)}
	}
	p := &parser{budget: maxNodes}
	cfg := p.config(&doc)
	if len(p.errs) > 0 {
		slices.SortStableFunc(p.errs, func(a, b Error) int {
			if a.Line != b.Line {
				return a.Line - b.Line
			}
			return a.Column - b.Column
		})
		return nil, p.errs
	}
	return cfg, nil
}

// yamlError turns a YAML syntax error into an Error with its line.
func yamlError(err error) Error {
	msg := strings.TrimSpace(err.Error())
	var te *yaml.TypeError
	if errors.As(err, &te) && len(te.Errors) > 0 {
		msg = te.Errors[0]
	}
	if m := yamlErrRE.FindStringSubmatch(msg); m != nil {
		line, _ := strconv.Atoi(m[1])
		return Error{Line: line, Message: "invalid YAML: " + m[2]}
	}
	return Error{Message: "invalid YAML: " + strings.TrimPrefix(msg, "yaml: ")}
}

type parser struct {
	errs   Errors
	budget int
}

func (p *parser) add(n *yaml.Node, path, format string, args ...any) {
	e := Error{Path: path, Message: fmt.Sprintf(format, args...)}
	if n != nil {
		e.Line, e.Column = n.Line, n.Column
	}
	p.errs = append(p.errs, e)
}

// resolve follows aliases and unwraps documents. It returns nil once the
// node budget is spent, after recording one error.
func (p *parser) resolve(n *yaml.Node) *yaml.Node {
	p.budget--
	if p.budget == 0 {
		p.add(nil, "", "the file is too complex (too many aliases or nodes)")
	}
	if p.budget <= 0 {
		return nil
	}
	for n != nil && (n.Kind == yaml.AliasNode || n.Kind == yaml.DocumentNode) {
		if n.Kind == yaml.AliasNode {
			n = n.Alias
		} else if len(n.Content) > 0 {
			n = n.Content[0]
		} else {
			return nil
		}
	}
	return n
}

// pair is one key and value of a mapping.
type pair struct{ key, value *yaml.Node }

// pairs returns the entries of mapping n in file order, with merge keys
// ("<<: *template") applied: keys written in n win over merged ones.
func (p *parser) pairs(n *yaml.Node, at string) []pair {
	var own, merged []pair
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Kind == yaml.ScalarNode && k.Tag == "!!merge" {
			merged = append(merged, p.mergeSources(v, at)...)
			continue
		}
		if k.Kind != yaml.ScalarNode {
			p.add(k, at, "keys must be plain strings")
			continue
		}
		if seen[k.Value] {
			p.add(k, join(at, k.Value), "is defined more than once")
			continue
		}
		seen[k.Value] = true
		own = append(own, pair{k, v})
	}
	for _, m := range merged {
		if !seen[m.key.Value] {
			seen[m.key.Value] = true
			own = append(own, m)
		}
	}
	return own
}

// mergeSources returns the entries a merge key brings in: from one mapping,
// or from a list of mappings where earlier ones win.
func (p *parser) mergeSources(v *yaml.Node, at string) []pair {
	v = p.resolve(v)
	if v == nil {
		return nil
	}
	switch v.Kind {
	case yaml.MappingNode:
		return p.pairs(v, at)
	case yaml.SequenceNode:
		var out []pair
		seen := map[string]bool{}
		for _, item := range v.Content {
			item = p.resolve(item)
			if item == nil || item.Kind != yaml.MappingNode {
				p.add(v, at, "<< must refer to a mapping or a list of mappings")
				continue
			}
			for _, kv := range p.pairs(item, at) {
				if !seen[kv.key.Value] {
					seen[kv.key.Value] = true
					out = append(out, kv)
				}
			}
		}
		return out
	}
	p.add(v, at, "<< must refer to a mapping or a list of mappings")
	return nil
}

func (p *parser) config(doc *yaml.Node) *Config {
	if doc.Kind == 0 {
		p.add(nil, "", "the file is empty")
		return nil
	}
	root := p.resolve(doc)
	if root == nil || (root.Kind == yaml.ScalarNode && root.Tag == "!!null") {
		p.add(nil, "", "the file is empty")
		return nil
	}
	if root.Kind != yaml.MappingNode {
		p.add(root, "", "the file must be a mapping of stages, variables and jobs")
		return nil
	}

	cfg := &Config{Stages: slices.Clone(DefaultStages)}
	var jobs []pair
	for _, kv := range p.pairs(root, "") {
		key := kv.key.Value
		switch {
		case key == "stages":
			cfg.Stages = p.stages(kv.value)
		case key == "variables":
			cfg.Variables = p.variables(kv.value, "variables")
		case strings.HasPrefix(key, "."):
			// A template for anchors, not a job.
		case unsupportedTop[key]:
			p.add(kv.key, key, "is not supported in .git2-ci.yml v0")
		default:
			jobs = append(jobs, kv)
		}
	}

	stageIndex := make(map[string]int, len(cfg.Stages))
	for i, s := range cfg.Stages {
		stageIndex[s] = i
	}
	for _, kv := range jobs {
		if job, ok := p.job(kv, stageIndex); ok {
			cfg.Jobs = append(cfg.Jobs, job)
		}
	}
	switch {
	case len(jobs) == 0:
		p.add(root, "", "the file defines no jobs")
	case len(jobs) > MaxJobs:
		p.add(root, "", "the file defines %d jobs; at most %d are allowed", len(jobs), MaxJobs)
	}
	return cfg
}

func (p *parser) stages(v *yaml.Node) []string {
	n := p.resolve(v)
	if n == nil || n.Kind != yaml.SequenceNode || len(n.Content) == 0 {
		p.add(v, "stages", "must be a non-empty list of stage names")
		return nil
	}
	if len(n.Content) > MaxStages {
		p.add(n, "stages", "at most %d stages are allowed", MaxStages)
	}
	var out []string
	for i, item := range n.Content {
		at := fmt.Sprintf("stages[%d]", i)
		s, ok := p.scalar(item, at)
		if !ok {
			continue
		}
		switch {
		case !nameRE.MatchString(s):
			p.add(item, at, "stage names are 1 to 63 characters from A-Z, a-z, 0-9, \"_\", \".\" and \"-\", starting with a letter or digit")
		case slices.Contains(out, s):
			p.add(item, at, "stage %q is listed more than once", s)
		default:
			out = append(out, s)
		}
	}
	return out
}

func (p *parser) job(kv pair, stageIndex map[string]int) (Job, bool) {
	name := kv.key.Value
	errsBefore := len(p.errs)
	if !nameRE.MatchString(name) {
		p.add(kv.key, name, "job names are 1 to 63 characters from A-Z, a-z, 0-9, \"_\", \".\" and \"-\", starting with a letter or digit")
	}
	n := p.resolve(kv.value)
	if n == nil || n.Kind != yaml.MappingNode {
		p.add(kv.value, name, "a job must be a mapping with at least image and script")
		return Job{}, false
	}

	job := Job{Name: name, Stage: DefaultStage, Timeout: DefaultTimeout}
	stageNode := kv.key
	var hasImage, hasScript bool
	for _, f := range p.pairs(n, name) {
		at := join(name, f.key.Value)
		switch f.key.Value {
		case "stage":
			stageNode = f.value
			if s, ok := p.scalar(f.value, at); ok {
				job.Stage = s
			}
		case "image":
			hasImage = true
			job.Image = p.image(f.value, at)
		case "script":
			hasScript = true
			job.Script = p.script(f.value, at)
		case "variables":
			job.Variables = p.variables(f.value, at)
		case "tags":
			job.Tags = p.tags(f.value, at)
		case "timeout":
			if d, ok := p.duration(f.value, at, MinTimeout, MaxTimeout); ok {
				job.Timeout = d
			}
		case "artifacts":
			job.Artifacts = p.artifacts(f.value, at)
		case "cache":
			job.Cache = p.cache(f.value, at)
		default:
			if unsupportedJob[f.key.Value] {
				p.add(f.key, at, "is not supported in .git2-ci.yml v0")
			} else {
				p.add(f.key, at, "unknown key; a job has stage, image, script, variables, tags, timeout, artifacts and cache")
			}
		}
	}
	if !hasImage {
		p.add(kv.key, name, "image is required")
	}
	if !hasScript {
		p.add(kv.key, name, "script is required")
	}
	if i, ok := stageIndex[job.Stage]; ok {
		job.StageIndex = i
	} else {
		p.add(stageNode, join(name, "stage"), "stage %q is not in stages", job.Stage)
	}
	return job, len(p.errs) == errsBefore
}

// scalar returns the text of a non-null scalar.
func (p *parser) scalar(v *yaml.Node, at string) (string, bool) {
	n := p.resolve(v)
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		p.add(v, at, "must be a single value")
		return "", false
	}
	return n.Value, true
}

func (p *parser) image(v *yaml.Node, at string) string {
	s, ok := p.scalar(v, at)
	switch {
	case !ok:
		return ""
	case s == "" || len(s) > 255 || strings.ContainsAny(s, " \t\r\n"):
		p.add(v, at, "must be an image reference such as golang:1.26, without spaces, at most 255 characters")
		return ""
	}
	return s
}

func (p *parser) script(v *yaml.Node, at string) []string {
	n := p.resolve(v)
	if n == nil {
		return nil
	}
	if n.Kind == yaml.ScalarNode && n.Tag != "!!null" {
		if strings.TrimSpace(n.Value) == "" {
			p.add(n, at, "must not be empty")
			return nil
		}
		return []string{n.Value}
	}
	if n.Kind != yaml.SequenceNode || len(n.Content) == 0 {
		p.add(v, at, "must be a command or a non-empty list of commands")
		return nil
	}
	var out []string
	for i, item := range n.Content {
		itemAt := fmt.Sprintf("%s[%d]", at, i)
		r := p.resolve(item)
		if r == nil || r.Kind != yaml.ScalarNode || r.Tag == "!!null" {
			p.add(item, itemAt, "must be a command; quote lines that contain \": \" or start with a special character")
			continue
		}
		if strings.TrimSpace(r.Value) == "" {
			p.add(item, itemAt, "must not be empty")
			continue
		}
		out = append(out, r.Value)
	}
	return out
}

func (p *parser) variables(v *yaml.Node, at string) []Variable {
	n := p.resolve(v)
	if n == nil || n.Kind != yaml.MappingNode {
		p.add(v, at, "must be a mapping of names to values")
		return nil
	}
	entries := p.pairs(n, at)
	if len(entries) > MaxVariables {
		p.add(n, at, "at most %d variables are allowed", MaxVariables)
	}
	var out []Variable
	for _, kv := range entries {
		name := kv.key.Value
		nameAt := join(at, name)
		if !variableRE.MatchString(name) {
			p.add(kv.key, nameAt, "variable names are 1 to 100 characters from A-Z, a-z, 0-9 and \"_\", not starting with a digit")
			continue
		}
		if reserved(name) {
			p.add(kv.key, nameAt, "names starting with %s are reserved for git2", strings.Join(ReservedPrefixes, " or "))
			continue
		}
		val, ok := p.scalar(kv.value, nameAt)
		if !ok {
			continue
		}
		if len(val) > MaxValueSize || strings.ContainsRune(val, 0) {
			p.add(kv.value, nameAt, "values are at most %d KiB and contain no NUL character", MaxValueSize>>10)
			continue
		}
		out = append(out, Variable{Name: name, Value: val})
	}
	return out
}

// reserved reports whether a variable name uses a prefix git2 keeps for
// itself.
func reserved(name string) bool {
	for _, prefix := range ReservedPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func (p *parser) tags(v *yaml.Node, at string) []string {
	n := p.resolve(v)
	if n == nil || n.Kind != yaml.SequenceNode {
		p.add(v, at, "must be a list of runner labels")
		return nil
	}
	if len(n.Content) > MaxTags {
		p.add(n, at, "at most %d tags are allowed", MaxTags)
	}
	var out []string
	for i, item := range n.Content {
		itemAt := fmt.Sprintf("%s[%d]", at, i)
		s, ok := p.scalar(item, itemAt)
		switch {
		case !ok:
		case !tagRE.MatchString(s):
			p.add(item, itemAt, "tags are 1 to 63 characters from A-Z, a-z, 0-9, \"_\", \".\" and \"-\"")
		case slices.Contains(out, s):
			p.add(item, itemAt, "tag %q is listed more than once", s)
		default:
			out = append(out, s)
		}
	}
	return out
}

func (p *parser) duration(v *yaml.Node, at string, lo, hi time.Duration) (time.Duration, bool) {
	s, ok := p.scalar(v, at)
	if !ok {
		return 0, false
	}
	d, err := ParseDuration(s)
	if err != nil {
		p.add(v, at, "must be a duration such as 90s, 10m, 1h30m or 7d")
		return 0, false
	}
	if d < lo || d > hi {
		p.add(v, at, "must be between %s and %s", FormatDuration(lo), FormatDuration(hi))
		return 0, false
	}
	return d, true
}

func (p *parser) artifacts(v *yaml.Node, at string) *Artifacts {
	n := p.resolve(v)
	if n == nil || n.Kind != yaml.MappingNode {
		p.add(v, at, "must be a mapping with paths and, optionally, expire_in")
		return nil
	}
	a := &Artifacts{ExpireIn: DefaultExpireIn}
	hasPaths := false
	for _, f := range p.pairs(n, at) {
		fAt := join(at, f.key.Value)
		switch f.key.Value {
		case "paths":
			hasPaths = true
			a.Paths = p.paths(f.value, fAt)
		case "expire_in":
			if d, ok := p.duration(f.value, fAt, MinExpireIn, MaxExpireIn); ok {
				a.ExpireIn = d
			}
		default:
			p.add(f.key, fAt, "unknown key; artifacts have paths and expire_in")
		}
	}
	if !hasPaths {
		p.add(v, at, "paths is required")
	}
	return a
}

func (p *parser) cache(v *yaml.Node, at string) *Cache {
	n := p.resolve(v)
	if n == nil || n.Kind != yaml.MappingNode {
		p.add(v, at, "must be a mapping with paths and, optionally, key")
		return nil
	}
	c := &Cache{}
	hasPaths := false
	for _, f := range p.pairs(n, at) {
		fAt := join(at, f.key.Value)
		switch f.key.Value {
		case "paths":
			hasPaths = true
			c.Paths = p.paths(f.value, fAt)
		case "key":
			if s, ok := p.scalar(f.value, fAt); ok {
				if !tagRE.MatchString(s) {
					p.add(f.value, fAt, "cache keys are 1 to 63 characters from A-Z, a-z, 0-9, \"_\", \".\" and \"-\"")
				}
				c.Key = s
			}
		default:
			p.add(f.key, fAt, "unknown key; a cache has paths and key")
		}
	}
	if !hasPaths {
		p.add(v, at, "paths is required")
	}
	return c
}

// paths validates artifact and cache paths: relative to the job's working
// directory and staying inside it. Glob patterns are allowed.
func (p *parser) paths(v *yaml.Node, at string) []string {
	n := p.resolve(v)
	if n == nil || n.Kind != yaml.SequenceNode || len(n.Content) == 0 {
		p.add(v, at, "must be a non-empty list of paths")
		return nil
	}
	if len(n.Content) > MaxPaths {
		p.add(n, at, "at most %d paths are allowed", MaxPaths)
	}
	var out []string
	for i, item := range n.Content {
		itemAt := fmt.Sprintf("%s[%d]", at, i)
		s, ok := p.scalar(item, itemAt)
		if !ok {
			continue
		}
		clean := path.Clean(s)
		if s == "" || len(s) > 255 || path.IsAbs(s) || clean == ".." || strings.HasPrefix(clean, "../") {
			p.add(item, itemAt, "must be a relative path inside the working directory, at most 255 characters")
			continue
		}
		out = append(out, s)
	}
	return out
}

// ParseDuration parses durations such as "90s", "10m", "1h30m" and "7d":
// whole numbers of days, hours, minutes and seconds, in that order.
func ParseDuration(s string) (time.Duration, error) {
	m := durationRE.FindStringSubmatch(s)
	if s == "" || m == nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	units := []time.Duration{24 * time.Hour, time.Hour, time.Minute, time.Second}
	var total time.Duration
	for i, unit := range units {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.ParseInt(m[i+1], 10, 32)
		if err != nil || time.Duration(n) > (1<<62)/unit {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		total += time.Duration(n) * unit
	}
	return total, nil
}

// FormatDuration writes d in the same form ParseDuration reads, for
// example "1h30m" or "7d".
func FormatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	var b strings.Builder
	for _, u := range []struct {
		unit   time.Duration
		suffix string
	}{{24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}} {
		if n := d / u.unit; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, u.suffix)
			d -= n * u.unit
		}
	}
	return b.String()
}

func join(at, key string) string {
	if at == "" {
		return key
	}
	return at + "." + key
}
