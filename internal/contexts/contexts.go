// Package contexts loads named workspace contexts from config.yaml.
//
// A context bundles a working directory with an optional model and effort, so
// the agent runs in the right workspace.
// config.yaml is mandatory — iu will not boot without at least one context.
// Secrets and infra connection stay in .env (internal/config); only this
// structured map lives in YAML.
//
// A user selects another workspace with a #alias. Unknown top-level keys are
// tolerated while unknown keys inside bot-owned blocks fail fast.
package contexts

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

var effortLevels = []string{"low", "medium", "high", "xhigh", "max"}

func parseJobCron(spec string) (cron.Schedule, error) {
	if (strings.HasPrefix(spec, "TZ=") || strings.HasPrefix(spec, "CRON_TZ=")) &&
		!strings.Contains(spec, " ") {
		return nil, errors.New("timezone prefix must be followed by a cron expression")
	}
	return cron.ParseStandard(spec)
}

// SplitProviderModel splits a context's model ("provider/model") into its
// parts. It returns ("", "") when no model is set (opencode then uses its own
// default). A non-empty value must be provider-qualified.
func SplitProviderModel(model string) (provider, modelID string, err error) {
	if model == "" {
		return "", "", nil
	}
	if !strings.Contains(model, "/") {
		return "", "", fmt.Errorf(
			"model %q must be 'provider/model' (e.g. 'anthropic/claude-opus-4-8')", model)
	}
	provider, modelID, _ = strings.Cut(model, "/")
	return provider, modelID, nil
}

// ContextConfig is one named workspace the agent can act in.
type ContextConfig struct {
	Directory   string `yaml:"directory"`
	Description string `yaml:"description"`
	Model       string `yaml:"model"`
	Effort      string `yaml:"effort"`
}

// ProviderModel returns (provider, model) for the opencode prompt body, or
// ("", "") when no model is configured. The value was validated at load time.
func (c ContextConfig) ProviderModel() (provider, modelID string) {
	provider, modelID, _ = SplitProviderModel(c.Model)
	return provider, modelID
}

// ContextsConfig is the contexts section of config.yaml: a set of contexts
// plus the default.
type ContextsConfig struct {
	DefaultContext string
	Contexts       map[string]ContextConfig
}

// Has reports whether name is a defined context (a valid #alias selector).
func (c *ContextsConfig) Has(name string) bool {
	_, ok := c.Contexts[name]
	return name != "" && ok
}

// Get resolves a context by name, falling back to DefaultContext when the
// name is unset or no longer defined.
func (c *ContextsConfig) Get(name string) ContextConfig {
	if ctx, ok := c.Contexts[name]; ok && name != "" {
		return ctx
	}
	return c.Contexts[c.DefaultContext]
}

// ResolveName is the context name Get would use — for persisting the binding.
func (c *ContextsConfig) ResolveName(name string) string {
	if c.Has(name) {
		return name
	}
	return c.DefaultContext
}

// ConfigError is raised when config.yaml is missing or malformed.
type ConfigError struct {
	msg string
}

func (e *ConfigError) Error() string { return e.msg }

func missingError(path string) *ConfigError {
	return &ConfigError{msg: fmt.Sprintf(
		"No contexts config found at %s. config.yaml is required: it must "+
			"define default_context and at least one context.", path)}
}

func invalidError(path string, problems []string) *ConfigError {
	var b strings.Builder
	fmt.Fprintf(&b, "Invalid contexts config at %s:", path)
	for _, p := range problems {
		fmt.Fprintf(&b, "\n  - %s", p)
	}
	return &ConfigError{msg: b.String()}
}

// strictDecode decodes a YAML node into out, rejecting unknown fields (the
// typo protection pydantic's extra="forbid" provided).
func strictDecode(node *yaml.Node, out any) error {
	raw, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	return dec.Decode(out)
}

// isAbsent reports whether a yaml.Node is missing or explicit null.
func isAbsent(node *yaml.Node) bool {
	return node == nil || node.Kind == 0 ||
		(node.Kind == yaml.ScalarNode && node.Tag == "!!null")
}

// FileConfig contains all bot-owned settings from config.yaml.
type FileConfig struct {
	Contexts  *ContextsConfig
	Reactions *ReactionsConfig
	QA        *QaSettings
	Jobs      map[string]JobConfig
}

// JobConfig defines one recurring prompt from config.yaml.
type JobConfig struct {
	Cron    string `yaml:"cron"`
	Context string `yaml:"context"`
	ChatID  string `yaml:"chat_id"`
	Prompt  string `yaml:"prompt"`
}

// Load reads and decodes config.yaml once, then validates each bot-owned block.
func Load(path string) (*FileConfig, error) {
	top, err := loadTop(path, true)
	if err != nil {
		return nil, err
	}
	ctxs, err := parseContexts(path, top)
	if err != nil {
		return nil, err
	}
	reactions, err := parseReactions(path, top["reactions"])
	if err != nil {
		return nil, err
	}
	qa, err := parseQaSettings(path, top["qa"])
	if err != nil {
		return nil, err
	}
	jobs, err := parseJobs(path, top["jobs"], ctxs)
	if err != nil {
		return nil, err
	}
	return &FileConfig{Contexts: ctxs, Reactions: reactions, QA: qa, Jobs: jobs}, nil
}

func parseJobs(path string, node yaml.Node, contexts *ContextsConfig) (map[string]JobConfig, error) {
	if isAbsent(&node) {
		return map[string]JobConfig{}, nil
	}

	jobs := map[string]JobConfig{}
	if err := strictDecode(&node, &jobs); err != nil {
		return nil, invalidError(path, []string{"jobs: " + yamlErr(err)})
	}

	var problems []string
	for name, job := range jobs {
		problems = append(problems, validateJob(name, job, contexts)...)
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, invalidError(path, problems)
	}
	return jobs, nil
}

func validateJob(name string, job JobConfig, contexts *ContextsConfig) []string {
	var problems []string
	if strings.TrimSpace(name) == "" {
		problems = append(problems, "jobs: job name must not be blank")
	}
	if strings.TrimSpace(job.Cron) == "" {
		problems = append(problems, fmt.Sprintf("jobs.%s.cron: field required", name))
	} else if _, err := parseJobCron(job.Cron); err != nil {
		problems = append(problems, fmt.Sprintf("jobs.%s.cron: invalid cron expression: %v", name, err))
	}
	if strings.TrimSpace(job.Context) == "" {
		problems = append(problems, fmt.Sprintf("jobs.%s.context: field required", name))
	} else if !contexts.Has(job.Context) {
		problems = append(problems, fmt.Sprintf("jobs.%s.context: context %q not found", name, job.Context))
	}
	if strings.TrimSpace(job.ChatID) == "" {
		problems = append(problems, fmt.Sprintf("jobs.%s.chat_id: field required", name))
	} else if job.ChatID != strings.TrimSpace(job.ChatID) {
		problems = append(problems, fmt.Sprintf("jobs.%s.chat_id: must not contain surrounding whitespace", name))
	}
	if strings.TrimSpace(job.Prompt) == "" {
		problems = append(problems, fmt.Sprintf("jobs.%s.prompt: field required", name))
	}
	return problems
}

// LoadContexts loads and validates the mandatory config.yaml. A missing or
// invalid file returns a *ConfigError, which the boot sequence turns into a
// fatal, single-message exit.
func LoadContexts(path string) (*ContextsConfig, error) {
	top, err := loadTop(path, true)
	if err != nil {
		return nil, err
	}
	return parseContexts(path, top)
}

func parseContexts(path string, top map[string]yaml.Node) (*ContextsConfig, error) {
	var defaultContext *string
	defaultKey := "default_context"
	if _, ok := top[defaultKey]; !ok {
		defaultKey = "default_project"
	}
	if node, ok := top[defaultKey]; ok && !isAbsent(&node) {
		var value string
		if err := node.Decode(&value); err != nil {
			return nil, invalidError(path, []string{defaultKey + ": " + yamlErr(err)})
		}
		defaultContext = &value
	}
	contextKey := "contexts"
	if _, ok := top[contextKey]; !ok {
		contextKey = "projects"
	}
	var contextNodes map[string]yaml.Node
	if node, ok := top[contextKey]; ok && !isAbsent(&node) {
		if err := node.Decode(&contextNodes); err != nil {
			return nil, invalidError(path, []string{contextKey + ": " + yamlErr(err)})
		}
	}

	var problems []string
	cfg := &ContextsConfig{Contexts: map[string]ContextConfig{}}

	for name, node := range contextNodes {
		var cc ContextConfig
		if err := strictDecode(&node, &cc); err != nil {
			problems = append(problems, fmt.Sprintf("contexts.%s: %s", name, yamlErr(err)))
			continue
		}
		problems = append(problems, validateContext(name, cc)...)
		cfg.Contexts[name] = cc
	}

	switch {
	case defaultContext == nil || *defaultContext == "":
		problems = append(problems, "default_context: field required")
	default:
		cfg.DefaultContext = *defaultContext
		if len(contextNodes) == 0 {
			problems = append(problems, "(root): contexts must define at least one context")
		} else if _, ok := contextNodes[cfg.DefaultContext]; !ok {
			names := make([]string, 0, len(contextNodes))
			for name := range contextNodes {
				names = append(names, name)
			}
			sort.Strings(names)
			problems = append(problems, fmt.Sprintf(
				"(root): default_context %q not found in contexts: %v", cfg.DefaultContext, names))
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems) // map iteration order is random; keep output stable
		return nil, invalidError(path, problems)
	}
	return cfg, nil
}

func validateContext(name string, cc ContextConfig) []string {
	var problems []string
	switch {
	case cc.Directory == "":
		problems = append(problems, fmt.Sprintf("projects.%s.directory: field required", name))
	case !filepath.IsAbs(cc.Directory):
		problems = append(problems, fmt.Sprintf(
			"projects.%s.directory: must be an absolute path, got %q", name, cc.Directory))
	default:
		root := filepath.Clean("/home/tray/projects")
		directory := filepath.Clean(cc.Directory)
		rel, err := filepath.Rel(root, directory)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			problems = append(problems, fmt.Sprintf(
				"projects.%s.directory: must be under %s, got %q", name, root, cc.Directory))
		}
	}
	if _, _, err := SplitProviderModel(cc.Model); err != nil {
		problems = append(problems, fmt.Sprintf("contexts.%s.model: %s", name, err))
	}
	if cc.Effort != "" && !slices.Contains(effortLevels, cc.Effort) {
		sorted := append([]string{}, effortLevels...)
		sort.Strings(sorted)
		problems = append(problems, fmt.Sprintf(
			"contexts.%s.effort: effort must be one of %v, got %q", name, sorted, cc.Effort))
	}
	return problems
}

// yamlErr strips the "yaml: " prefix the decoder adds and flattens its
// multi-line unmarshal reports, keeping problem lines uniform with the
// field-level messages.
func yamlErr(err error) string {
	msg := strings.TrimPrefix(err.Error(), "yaml: ")
	msg = strings.TrimPrefix(msg, "unmarshal errors:\n")
	lines := strings.Split(msg, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	return strings.Join(lines, "; ")
}

// ReactionsConfig holds the emoji names iu uses for its reaction lifecycle
// (config.yaml → reactions:). Values are Lark emoji_type names (e.g.
// OneSecond, CrossMark). An empty list means no emoji triggers deletion.
type ReactionsConfig struct {
	// Processing is added while a turn is in progress (the "received, working" ack).
	Processing string `yaml:"processing"`
	// Negative is the list of emoji types that trigger message deletion when a
	// user reacts with them on a message iu sent.
	Negative []string `yaml:"negative"`
}

// DefaultReactions returns the default emoji set (empty negative list — config
// must opt in).
func DefaultReactions() *ReactionsConfig {
	return &ReactionsConfig{Processing: "OneSecond"}
}

// LoadReactions loads the optional reactions: block from config.yaml.
//
// Lenient: a missing file or absent block yields the defaults (the loader for
// contexts already fails fast if the file is missing). An invalid block
// returns a *ConfigError so a typo is caught at boot.
func LoadReactions(path string) (*ReactionsConfig, error) {
	top, err := loadTop(path, false)
	if err != nil {
		return nil, err
	}
	return parseReactions(path, top["reactions"])
}

func parseReactions(path string, node yaml.Node) (*ReactionsConfig, error) {
	r := DefaultReactions()
	if isAbsent(&node) {
		return r, nil
	}
	if err := strictDecode(&node, r); err != nil {
		return nil, invalidError(path, []string{"reactions: " + yamlErr(err)})
	}
	return r, nil
}

// QaSettings hold long-run behaviour for agent turns (config.yaml → qa): a
// hard turn timeout and the question-card answer window. Lark run-status
// timing is product behavior, not deployment configuration.
type QaSettings struct {
	TurnTimeoutMinutes     float64
	QuestionTimeoutMinutes float64
}

// TurnTimeout is the hard per-turn deadline.
func (q *QaSettings) TurnTimeout() time.Duration {
	return time.Duration(q.TurnTimeoutMinutes * float64(time.Minute))
}

// QuestionTimeout is how long a question card waits for a human answer while
// the turn clock is suspended (0 = no explicit window; the streamer caps any
// wait at 24 hours so an unanswered card can't suspend a turn forever).
func (q *QaSettings) QuestionTimeout() time.Duration {
	return time.Duration(q.QuestionTimeoutMinutes * float64(time.Minute))
}

// DefaultQaSettings returns the defaults applied when the qa block is absent.
func DefaultQaSettings() *QaSettings {
	return &QaSettings{
		TurnTimeoutMinutes:     60,
		QuestionTimeoutMinutes: 30,
	}
}

// LoadQaSettings loads the optional qa: block from config.yaml (lenient like
// LoadReactions: an absent block yields defaults; a malformed one returns a
// *ConfigError so a typo is caught at boot).
func LoadQaSettings(path string) (*QaSettings, error) {
	top, err := loadTop(path, false)
	if err != nil {
		return nil, err
	}
	return parseQaSettings(path, top["qa"])
}

func parseQaSettings(path string, node yaml.Node) (*QaSettings, error) {
	q := DefaultQaSettings()
	if isAbsent(&node) {
		return q, nil
	}
	var raw struct {
		TurnTimeoutMinutes     *float64 `yaml:"turn_timeout_minutes"`
		QuestionTimeoutMinutes *float64 `yaml:"question_timeout_minutes"`
	}
	if err := strictDecode(&node, &raw); err != nil {
		return nil, invalidError(path, []string{"qa: " + yamlErr(err)})
	}
	if raw.TurnTimeoutMinutes != nil {
		q.TurnTimeoutMinutes = *raw.TurnTimeoutMinutes
	}
	if raw.QuestionTimeoutMinutes != nil {
		q.QuestionTimeoutMinutes = *raw.QuestionTimeoutMinutes
	}
	var problems []string
	if !isFinite(q.TurnTimeoutMinutes) || q.TurnTimeoutMinutes <= 0 {
		problems = append(problems, "qa.turn_timeout_minutes: must be greater than 0")
	}
	if !isFinite(q.QuestionTimeoutMinutes) || q.QuestionTimeoutMinutes < 0 || q.QuestionTimeoutMinutes > 24*60 {
		problems = append(problems, "qa.question_timeout_minutes: must be between 0 and 1440")
	}
	if len(problems) > 0 {
		return nil, invalidError(path, problems)
	}
	return q, nil
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func loadTop(path string, required bool) (map[string]yaml.Node, error) {
	if path == "" {
		if required {
			return nil, missingError("config.yaml")
		}
		return map[string]yaml.Node{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if required {
			return nil, missingError(path)
		}
		return map[string]yaml.Node{}, nil
	}
	var top map[string]yaml.Node
	if err := yaml.Unmarshal(data, &top); err != nil {
		return nil, invalidError(path, []string{yamlErr(err)})
	}
	return top, nil
}
