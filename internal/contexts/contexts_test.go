package contexts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadsSingleContext(t *testing.T) {
	p := write(t, `
default_context: otp
contexts:
  otp:
    directory: /home/tray/project/otp
    model: anthropic/claude-opus-4-8
    effort: high
`)
	cfg, err := LoadContexts(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx := cfg.Get("")
	if ctx.Directory != "/home/tray/project/otp" {
		t.Errorf("Directory = %q", ctx.Directory)
	}
	provider, model := ctx.ProviderModel()
	if provider != "anthropic" || model != "claude-opus-4-8" {
		t.Errorf("ProviderModel = (%q, %q)", provider, model)
	}
	if ctx.Effort != "high" {
		t.Errorf("Effort = %q", ctx.Effort)
	}
}

func TestLoadReadsAllSettings(t *testing.T) {
	p := write(t, `
default_context: otp
contexts:
  otp:
    directory: /home/tray/project/otp
reactions:
  processing: Hourglass
qa:
  turn_timeout_minutes: 10
  question_timeout_minutes: 20
jobs:
  standup:
    cron: CRON_TZ=Asia/Singapore 0 9 * * 1-5
    context: otp
    chat_id: oc_team
    prompt: Use the lark-workflow-standup-report skill for today's report.
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Contexts.Get("").Directory != "/home/tray/project/otp" {
		t.Errorf("default directory = %q", cfg.Contexts.Get("").Directory)
	}
	if cfg.Reactions.Processing != "Hourglass" {
		t.Errorf("processing reaction = %q", cfg.Reactions.Processing)
	}
	if cfg.QA.TurnTimeout().Minutes() != 10 || cfg.QA.QuestionTimeout().Minutes() != 20 {
		t.Errorf("QA settings = %+v", cfg.QA)
	}
	if cfg.Jobs["standup"].ChatID != "oc_team" {
		t.Errorf("jobs = %+v", cfg.Jobs)
	}
}

func TestLoadJobsDefaultsToEmpty(t *testing.T) {
	p := write(t, "default_context: otp\ncontexts:\n  otp:\n    directory: /home/tray/project/otp\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Jobs == nil || len(cfg.Jobs) != 0 {
		t.Errorf("Jobs = %#v, want non-nil empty map", cfg.Jobs)
	}
}

func TestLoadJobsRejectsInvalidDefinitions(t *testing.T) {
	tests := []struct {
		name    string
		job     string
		problem string
	}{
		{name: "blank name", job: `"  ": {cron: "0 9 * * *", context: otp, chat_id: oc_team, prompt: report}`, problem: "job name must not be blank"},
		{name: "blank cron", job: `report: {cron: "  ", context: otp, chat_id: oc_team, prompt: report}`, problem: "cron: field required"},
		{name: "six-field cron", job: `report: {cron: "0 0 9 * * *", context: otp, chat_id: oc_team, prompt: report}`, problem: "invalid cron expression"},
		{name: "timezone without schedule", job: `report: {cron: "CRON_TZ=Asia/Singapore", context: otp, chat_id: oc_team, prompt: report}`, problem: "timezone prefix must be followed"},
		{name: "blank context", job: `report: {cron: "0 9 * * *", context: " ", chat_id: oc_team, prompt: report}`, problem: "context: field required"},
		{name: "unknown context", job: `report: {cron: "0 9 * * *", context: missing, chat_id: oc_team, prompt: report}`, problem: `context "missing" not found`},
		{name: "blank chat ID", job: `report: {cron: "0 9 * * *", context: otp, chat_id: " ", prompt: report}`, problem: "chat_id: field required"},
		{name: "padded chat ID", job: `report: {cron: "0 9 * * *", context: otp, chat_id: "oc_team ", prompt: report}`, problem: "must not contain surrounding whitespace"},
		{name: "blank prompt", job: `report: {cron: "0 9 * * *", context: otp, chat_id: oc_team, prompt: " "}`, problem: "prompt: field required"},
		{name: "unknown field", job: `report: {cron: "0 9 * * *", context: otp, chat_id: oc_team, prompt: report, typo: true}`, problem: "field typo not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := write(t, "default_context: otp\ncontexts:\n  otp:\n    directory: /home/tray/project/otp\njobs:\n  "+tt.job+"\n")
			_, err := Load(p)
			if err == nil || !strings.Contains(err.Error(), tt.problem) {
				t.Fatalf("Load() error = %v, want containing %q", err, tt.problem)
			}
		})
	}
}

func TestIgnoresUnknownTopLevelBlocks(t *testing.T) {
	// Top-level extension blocks are tolerated while context fields are strict.
	p := write(t, `
default_context: otp
contexts:
  otp:
    directory: /home/tray/project/otp
qa:
  turn_timeout_minutes: 60
extension:
  enabled: false
`)
	cfg, err := LoadContexts(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Get("").Directory != "/home/tray/project/otp" {
		t.Errorf("Directory = %q", cfg.Get("").Directory)
	}
}

func TestRejectsUnknownKeyInsideContext(t *testing.T) {
	p := write(t, `
default_context: otp
contexts:
  otp:
    directory: /home/tray/project/otp
    typo_field: oops
`)
	if _, err := LoadContexts(p); err == nil {
		t.Fatal("LoadContexts succeeded, want error")
	}
}

func TestRejectsDefaultNotInContexts(t *testing.T) {
	p := write(t, `
default_context: missing
contexts:
  otp:
    directory: /home/tray/project/otp
`)
	if _, err := LoadContexts(p); err == nil {
		t.Fatal("LoadContexts succeeded, want error")
	}
}

func TestMissingFileFails(t *testing.T) {
	if _, err := LoadContexts("/nonexistent/config.yaml"); err == nil {
		t.Fatal("LoadContexts succeeded, want error")
	}
}

func TestSplitProviderModelRequiresQualification(t *testing.T) {
	if p, m, err := SplitProviderModel(""); p != "" || m != "" || err != nil {
		t.Errorf("SplitProviderModel(\"\") = (%q, %q, %v)", p, m, err)
	}
	p, m, err := SplitProviderModel("anthropic/claude-opus-4-8")
	if p != "anthropic" || m != "claude-opus-4-8" || err != nil {
		t.Errorf("SplitProviderModel = (%q, %q, %v)", p, m, err)
	}
	if _, _, err := SplitProviderModel("claude-opus-4-8"); err == nil ||
		!strings.Contains(err.Error(), "provider/model") {
		t.Errorf("SplitProviderModel error = %v, want provider/model complaint", err)
	}
}

func repoPath(t *testing.T, rel string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("../..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRealConfigYamlLoads(t *testing.T) {
	// The committed example config (the template for config.yaml) must be valid.
	fileCfg, err := Load(repoPath(t, "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := fileCfg.Contexts
	if cfg.DefaultContext != "demo" {
		t.Errorf("DefaultContext = %q", cfg.DefaultContext)
	}
	if cfg.Get("").Directory != "/home/tray/project/demo" {
		t.Errorf("default directory = %q", cfg.Get("").Directory)
	}
	if fileCfg.Jobs == nil {
		t.Error("Jobs is nil, want empty map")
	}
}

func TestLoadReactionsDefaultsWhenAbsent(t *testing.T) {
	// A file with no reactions: block falls back to the defaults.
	p := write(t, "default_context: otp\ncontexts: {}\n")
	r, err := LoadReactions(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Processing != "OneSecond" || len(r.Negative) != 0 {
		t.Errorf("reactions = %+v", r)
	}
}

func TestLoadReactionsReadsOverrides(t *testing.T) {
	p := write(t, `
reactions:
  processing: Hourglass
  negative: [Wastebasket, CrossMark]
`)
	r, err := LoadReactions(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Processing != "Hourglass" {
		t.Errorf("reactions = %+v", r)
	}
	if len(r.Negative) != 2 || r.Negative[0] != "Wastebasket" || r.Negative[1] != "CrossMark" {
		t.Errorf("negative = %v, want [Wastebasket CrossMark]", r.Negative)
	}
}

func TestLoadReactionsRejectsUnknownKey(t *testing.T) {
	p := write(t, "reactions:\n  bogus: X\n")
	if _, err := LoadReactions(p); err == nil {
		t.Fatal("LoadReactions succeeded, want error")
	}
}

func TestRealConfigYamlReactionsLoad(t *testing.T) {
	r, err := LoadReactions(repoPath(t, "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if r.Processing != "OneSecond" {
		t.Errorf("reactions = %+v", r)
	}
	if len(r.Negative) != 0 {
		t.Errorf("negative = %v, want empty", r.Negative)
	}
}

func TestLoadQaSettingsDefaultsWhenAbsent(t *testing.T) {
	p := write(t, "default_context: otp\ncontexts: {}\n")
	q, err := LoadQaSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	if q.TurnTimeoutMinutes != 60 {
		t.Errorf("TurnTimeoutMinutes = %v", q.TurnTimeoutMinutes)
	}
	if q.QuestionTimeoutMinutes != 30 {
		t.Errorf("QuestionTimeoutMinutes = %v", q.QuestionTimeoutMinutes)
	}
}

func TestLoadQaSettingsReadsQuestionTimeout(t *testing.T) {
	p := write(t, "qa:\n  question_timeout_minutes: 7.5\n")
	q, err := LoadQaSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := q.QuestionTimeout().Minutes(); got != 7.5 {
		t.Errorf("QuestionTimeout = %v minutes, want 7.5", got)
	}
}

func TestLoadQaSettingsRejectsUnknownKey(t *testing.T) {
	p := write(t, `
qa:
  turn_timeout_minutes: 10
  typo: ignored
`)
	if _, err := LoadQaSettings(p); err == nil {
		t.Fatal("LoadQaSettings succeeded, want error")
	}
}

func TestLoadQaSettingsRejectsInvalidDurations(t *testing.T) {
	tests := []string{
		"qa:\n  turn_timeout_minutes: 0\n",
		"qa:\n  turn_timeout_minutes: .nan\n",
		"qa:\n  turn_timeout_minutes: .inf\n",
		"qa:\n  question_timeout_minutes: -1\n",
		"qa:\n  question_timeout_minutes: 1441\n",
		"qa:\n  question_timeout_minutes: .nan\n",
	}
	for _, body := range tests {
		if _, err := LoadQaSettings(write(t, body)); err == nil {
			t.Errorf("LoadQaSettings(%q) succeeded, want error", body)
		}
	}
}

func TestRealConfigYamlQaLoads(t *testing.T) {
	q, err := LoadQaSettings(repoPath(t, "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if q.TurnTimeout().Minutes() != 60 {
		t.Errorf("TurnTimeout = %v", q.TurnTimeout())
	}
}
