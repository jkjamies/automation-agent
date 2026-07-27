package setup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ollama/ollama/api"
)

// ErrOllamaUnreachable reports that the Ollama server could not be contacted at all, as
// distinct from being contacted and lacking a model. The two want opposite responses: a server
// that is not up yet is ordinary during local development and must not block startup, while a
// server that is up and does not have the configured model is a configuration error that will
// fail every run until someone fixes it.
var ErrOllamaUnreachable = errors.New("ollama server unreachable")

// preflightTimeout bounds the inventory call. It only lists locally-present models, so it is
// fast when the server is healthy; the bound exists so a wedged server cannot hang startup.
const preflightTimeout = 5 * time.Second

// VerifyOllamaModels reports whether every configured tag is present on the Ollama server.
//
// It exists because nothing else fails early: NewOllamaModel only builds a client, so a tag
// that was never pulled — or a family that was renamed between releases — constructs fine and
// first errors on the initial generation. By then a webhook has been accepted, a task
// dispatched, and a repository cloned, and the failure surfaces as an opaque drive error deep
// inside an agent run. Checking at startup turns a silent misconfiguration into one message
// naming the tag and the command that fixes it.
//
// Duplicate and empty tags are skipped, so callers can pass the base and code models directly
// without pre-filtering (they are frequently the same, and the code tier may be unset).
func VerifyOllamaModels(ctx context.Context, host string, tags ...string) error {
	wanted := dedupeTags(tags)
	if len(wanted) == 0 {
		return nil
	}
	client, err := preflightClient(host)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()

	list, err := client.List(ctx)
	if err != nil {
		return fmt.Errorf("%w at %s: %w", ErrOllamaUnreachable, host, err)
	}
	present := map[string]bool{}
	for _, m := range list.Models {
		present[normalizeTag(m.Name)] = true
	}
	var missing []string
	for _, tag := range wanted {
		if !present[normalizeTag(tag)] {
			missing = append(missing, tag)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("ollama at %s has no model %s (pull it with: %s)%s",
		host, strings.Join(quoteAll(missing), ", "),
		strings.Join(pullCommands(missing), " && "), availableSuffix(present))
}

// preflightClient builds a bare Ollama client for the inventory call. It is separate from the
// generation client on purpose: this request is small and must not inherit the 300s
// first-chunk cushion a cold model load needs.
func preflightClient(host string) (*api.Client, error) {
	base, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("parse ollama host %q: %w", host, err)
	}
	return api.NewClient(base, &http.Client{Timeout: preflightTimeout}), nil
}

// normalizeTag applies Ollama's implicit ":latest": a tag written without one refers to the
// same model the server lists as "<name>:latest", so comparing the raw strings would report a
// pulled model as missing.
func normalizeTag(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag != "" && !strings.Contains(tag, ":") {
		return tag + ":latest"
	}
	return tag
}

// dedupeTags drops blanks and repeats, preserving order so the error message lists the models
// in the order they were configured.
func dedupeTags(tags []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

func quoteAll(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[i] = fmt.Sprintf("%q", v)
	}
	return out
}

func pullCommands(missing []string) []string {
	out := make([]string, len(missing))
	for i, m := range missing {
		out[i] = "ollama pull " + m
	}
	return out
}

// availableSuffix lists what the server does have. A missing model is very often a typo or a
// renamed generation, and the inventory is usually short enough to make the right tag obvious —
// far more useful than "not found" on its own.
func availableSuffix(present map[string]bool) string {
	if len(present) == 0 {
		return "; the server has no models pulled at all"
	}
	names := make([]string, 0, len(present))
	for n := range present {
		names = append(names, n)
	}
	sort.Strings(names)
	return "; available: " + strings.Join(names, ", ")
}
