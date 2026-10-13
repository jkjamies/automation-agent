package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.TriggerLabel != "agent-ready" || c.Harness != "adk" || c.MaxAttempts != 3 || !c.DraftPRs ||
		c.GitHubAPI != "https://api.github.com" || c.PlannerModel != DefaultReasoningModel || c.CheckTimeout != 20*time.Minute {
		t.Errorf("defaults = %+v", c)
	}
	if err := c.RequireRun(); err == nil {
		t.Error("RequireRun passed with no credentials")
	}
}

func TestLoadValues(t *testing.T) {
	c, err := Load(env(map[string]string{
		"JIRA_BASE_URL":              "https://acme.atlassian.net/",
		"JIRA_API_TOKEN":             "t",
		"GITHUB_TOKEN":               "g",
		"GOOGLE_CLOUD_PROJECT":       "p",
		"TICKETFLOW_PROJECT_REPOS":   "proj=acme/app, WEB=acme/web",
		"TICKETFLOW_ALLOWED_REPOS":   "acme/lib",
		"TICKETFLOW_HARNESS":         "cli",
		"TICKETFLOW_HARNESS_COMMAND": "gemini -p {prompt}",
		"TICKETFLOW_HARNESS_ENV":     "GOOGLE_CLOUD_PROJECT,UNSET_VAR",
		"TICKETFLOW_DRAFT_PRS":       "false",
		"TICKETFLOW_MAX_ATTEMPTS":    "5",
		"TICKETFLOW_CHECK_TIMEOUT":   "90s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RequireRun(); err != nil {
		t.Errorf("RequireRun = %v", err)
	}
	if c.JiraBaseURL != "https://acme.atlassian.net" || c.ProjectRepos["PROJ"] != "acme/app" || c.ProjectRepos["WEB"] != "acme/web" {
		t.Errorf("parsed = %+v", c)
	}
	if strings.Join(c.HarnessCommand, " ") != "gemini -p {prompt}" || strings.Join(c.HarnessEnv, ",") != "GOOGLE_CLOUD_PROJECT=p" {
		t.Errorf("harness = %v %v", c.HarnessCommand, c.HarnessEnv)
	}
	if c.DraftPRs || c.MaxAttempts != 5 || c.CheckTimeout != 90*time.Second {
		t.Errorf("numbers = %+v", c)
	}
	for repo, want := range map[string]bool{"acme/app": true, "ACME/Web": true, "acme/lib": true, "evil/x": false} {
		if got := c.RepoAllowed(repo); got != want {
			t.Errorf("RepoAllowed(%s) = %v", repo, got)
		}
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	for k, v := range map[string]string{
		"TICKETFLOW_MAX_ATTEMPTS":  "lots",
		"TICKETFLOW_CHECK_TIMEOUT": "-1s",
		"TICKETFLOW_DRAFT_PRS":     "maybe",
		"TICKETFLOW_PROJECT_REPOS": "PROJ",
		"TICKETFLOW_HARNESS":       "cursor",
	} {
		if _, err := Load(env(map[string]string{k: v})); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%q: err = %v", k, v, err)
		}
	}
	if _, err := Load(env(map[string]string{"TICKETFLOW_HARNESS": "cli"})); err == nil {
		t.Error("cli harness without a command accepted")
	}
	if _, err := Load(env(map[string]string{"TICKETFLOW_MAX_ATTEMPTS": "0"})); err == nil {
		t.Error("zero attempts accepted")
	}
}
