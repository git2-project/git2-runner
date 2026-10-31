package pipeline

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	civ1 "github.com/git2-project/git2-runner/app/gen/go/ci/v1"
	"github.com/git2-project/git2-runner/app/internal/cisvc/apierr"
	"github.com/git2-project/git2-runner/app/internal/ciyaml"
)

func TestPipelineStatus(t *testing.T) {
	js := func(statuses ...JobStatus) []jobState {
		out := make([]jobState, len(statuses))
		for i, s := range statuses {
			out[i] = jobState{status: s}
		}
		return out
	}
	for _, tc := range []struct {
		jobs []jobState
		want Status
	}{
		{js(JobPending, JobCreated), StatusPending},
		{js(JobRunning, JobCreated), StatusRunning},
		{js(JobSuccess, JobPending), StatusRunning},
		{js(JobSuccess, JobSuccess), StatusSuccess},
		{js(JobSuccess, JobFailed, JobSkipped), StatusFailed},
		{js(JobCanceled, JobCanceled), StatusCanceled},
		{js(JobFailed, JobCanceled), StatusFailed},
		{js(JobCanceled, JobRunning), StatusRunning},
	} {
		if got := pipelineStatus(tc.jobs); got != tc.want {
			t.Errorf("pipelineStatus(%v) = %s, want %s", tc.jobs, got, tc.want)
		}
	}
}

func TestSpecOf(t *testing.T) {
	cfg, err := ciyaml.Parse([]byte(`
variables: {A: file, B: file}
j:
  image: alpine
  script: ["true"]
  variables: {B: job, C: job}
  timeout: 5m
  artifacts: {paths: [out/], expire_in: 1d}
`))
	if err != nil {
		t.Fatal(err)
	}
	spec := specOf(cfg, cfg.Jobs[0])
	var got []string
	for _, v := range spec.Variables {
		got = append(got, v.Name+"="+v.Value)
	}
	if strings.Join(got, " ") != "A=file B=job C=job" {
		t.Errorf("variables = %v", got)
	}
	if spec.TimeoutSeconds != 300 || spec.Artifacts.ExpireInSeconds != 86400 {
		t.Errorf("spec = %+v", spec)
	}
}

func TestValidateTrigger(t *testing.T) {
	ok := TriggerInput{
		RepositoryID: uuid.NewString(), Ref: "refs/tags/v1.0.0", CommitSHA: strings.Repeat("a", 64),
		Trigger: TriggerManual, TriggeredByID: uuid.NewString(), IdempotencyKey: "evt:refs/tags/v1.0.0",
	}
	if _, err := validateTrigger(ok); err != nil {
		t.Fatal(err)
	}
	// The recommended push key must pass for every valid ref, including
	// non-ASCII names and refs of the full 255 bytes.
	for _, ref := range []string{"refs/heads/feature/café", "refs/heads/" + strings.Repeat("ş", 122)} {
		in := ok
		in.Ref, in.IdempotencyKey = ref, uuid.NewString()+":"+ref
		if len(ref) > 255 {
			t.Fatalf("test ref is %d bytes", len(ref))
		}
		if _, err := validateTrigger(in); err != nil {
			t.Errorf("ref %q (%d bytes): %v", ref[:24], len(ref), err)
		}
	}
	for name, mutate := range map[string]func(*TriggerInput){
		"branch name only": func(in *TriggerInput) { in.Ref = "main" },
		"remote ref":       func(in *TriggerInput) { in.Ref = "refs/remotes/origin/main" },
		"ref with ..":      func(in *TriggerInput) { in.Ref = "refs/heads/a..b" },
		"ref with space":   func(in *TriggerInput) { in.Ref = "refs/heads/a b" },
		"upper-case sha":   func(in *TriggerInput) { in.CommitSHA = strings.Repeat("A", 40) },
		"short sha":        func(in *TriggerInput) { in.CommitSHA = "abc123" },
		"bad user":         func(in *TriggerInput) { in.TriggeredByID = "me" },
		"key too long":     func(in *TriggerInput) { in.IdempotencyKey = strings.Repeat("k", 513) },
		"key with newline": func(in *TriggerInput) { in.IdempotencyKey = "evt\nrefs/heads/main" },
		"key not UTF-8":    func(in *TriggerInput) { in.IdempotencyKey = "evt:\xff" },
		"ref not UTF-8":    func(in *TriggerInput) { in.Ref = "refs/heads/caf\xe9" },
	} {
		in := ok
		mutate(&in)
		if _, err := validateTrigger(in); apierr.KindOf(err) != apierr.Invalid {
			t.Errorf("%s: %v, want Invalid", name, err)
		}
	}
}

func TestValidateUpdate(t *testing.T) {
	zero, two := int32(0), int32(2)
	for _, tc := range []struct {
		in UpdateInput
		ok bool
	}{
		{UpdateInput{Status: JobRunning}, true},
		{UpdateInput{Status: JobSuccess, ExitCode: &zero}, true},
		{UpdateInput{Status: JobSuccess}, true},
		{UpdateInput{Status: JobSuccess, ExitCode: &two}, false},
		{UpdateInput{Status: JobFailed, Reason: ReasonScriptFailure, ExitCode: &two}, true},
		{UpdateInput{Status: JobFailed, Reason: ReasonScriptFailure}, false},
		{UpdateInput{Status: JobFailed, Reason: ReasonTimeout}, true},
		{UpdateInput{Status: JobFailed}, false},
		{UpdateInput{Status: JobFailed, Reason: ReasonRunnerLost}, false},
		{UpdateInput{Status: JobRunning, Reason: ReasonTimeout}, false},
		{UpdateInput{Status: JobCanceled}, false},
		{UpdateInput{}, false},
	} {
		if err := validateUpdate(tc.in); (err == nil) != tc.ok {
			t.Errorf("validateUpdate(%+v) = %v, want ok=%v", tc.in, err, tc.ok)
		}
	}
}

func TestConfigErrorMessage(t *testing.T) {
	_, err := ciyaml.Parse([]byte("j:\n  image: x\n  script: y\n  bad: 1\n"))
	if msg := configErrorMessage(err); msg != ".git2-ci.yml is invalid: line 4: j.bad: unknown key; a job has stage, image, script, variables, tags, timeout, artifacts and cache" {
		t.Errorf("message = %q", msg)
	}
	many := make(ciyaml.Errors, 12)
	if msg := configErrorMessage(many); !strings.HasSuffix(msg, "and 2 more") {
		t.Errorf("message = %q", msg)
	}
	if msg := configErrorMessage(errors.New("x")); msg != ".git2-ci.yml is invalid" {
		t.Errorf("message = %q", msg)
	}
}

func TestEnumsRoundTrip(t *testing.T) {
	for r := range reasonToProto {
		back, ok := ReasonFromProto(r.Proto())
		if !ok || back != r {
			t.Errorf("reason %q round-trips to %q", r, back)
		}
	}
	if _, ok := ReasonFromProto(civ1.JobFailureReason(99)); ok {
		t.Error("unknown reason accepted")
	}
	for _, s := range []JobStatus{JobCreated, JobPending, JobRunning, JobSuccess, JobFailed, JobCanceled, JobSkipped} {
		if s.Proto() == civ1.JobStatus_JOB_STATUS_UNSPECIFIED {
			t.Errorf("job status %q has no proto value", s)
		}
	}
	for _, s := range []Status{StatusPending, StatusRunning, StatusSuccess, StatusFailed, StatusCanceled} {
		if s.Proto() == civ1.PipelineStatus_PIPELINE_STATUS_UNSPECIFIED {
			t.Errorf("pipeline status %q has no proto value", s)
		}
	}
}
