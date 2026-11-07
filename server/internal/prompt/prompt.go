// Package prompt assembles everything the model sees, so the wording and the schema live in
// one testable place instead of being composed by the browser.
//
// The scene JSON Schema is not written by hand: `npm run schema` regenerates
// scene-schema.json from the zod validator in src/scene/schema.ts, and a test in this package
// fails if the committed artifact differs. The prompt therefore cannot describe a schema the
// browser does not enforce.
package prompt

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
)

// Version is the prompt version recorded on every generation row. Bump it whenever the wording
// or the embedded schema changes: cost and validity per prompt version is the only way to tell
// whether a change helped.
const Version = "2026-03-scene-v1"

//go:embed scene-schema.json templates/*.txt
var files embed.FS

// SchemaJSON is the embedded JSON Schema, exactly as generated from zod.
func SchemaJSON() []byte { return mustRead("scene-schema.json") }

// SchemaDocument is SchemaJSON parsed, for tests and for a future client-side validator.
func SchemaDocument() (map[string]any, error) {
	var doc map[string]any
	if err := json.Unmarshal(SchemaJSON(), &doc); err != nil {
		return nil, fmt.Errorf("prompt: the embedded scene schema is not valid JSON (run `npm run schema`): %w", err)
	}
	return doc, nil
}

// //go:embed of a nested directory needs the directory's own entry to exist; these helpers keep
// the path constants in one place.
const (
	systemTemplate = "templates/system.txt"
	repairTemplate = "templates/repair.txt"
	// ExamplePath is a file in the repo, not in this package: the worked example in the prompt
	// is the actual document the editor starts from (src/scene.json), so a change there is a
	// prompt change and the test says so.
)

func mustRead(name string) []byte {
	b, err := files.ReadFile(name)
	if err != nil {
		// A missing embedded file is a build error caught by the first test run, not a
		// runtime condition worth threading an error through every caller for.
		panic(fmt.Sprintf("prompt: embedded file %s is missing: %v", name, err))
	}
	return b
}

// System is the system prompt: schema, conventions, one worked example, output contract.
func System() string {
	// The schema is injected rather than pasted into the template so the two cannot disagree.
	return strings.ReplaceAll(string(mustRead(systemTemplate)), "{{SCHEMA}}", strings.TrimSpace(string(SchemaJSON())))
}

// Repair renders the repair instruction for a failed validation. It runs on the server so the
// wording is consistent and testable; the client only reports the issues.
//
// Issues arrive from the browser, which is untrusted by construction, so they are rendered as
// data with their paths prefixed, never concatenated into an instruction of their own.
func Repair(issues []Issue) string {
	if len(issues) == 0 {
		return ""
	}
	var b strings.Builder
	for _, issue := range issues {
		path := issue.Path
		if path == "" {
			path = "(document)"
		}
		// The path is bounded and quoted-ish so a malicious path cannot pose as an
		// instruction line. Anything newline-ish would break the list layout.
		b.WriteString("- ")
		b.WriteString(sanitizeLine(path))
		b.WriteString(": ")
		b.WriteString(sanitizeLine(issue.Message))
		b.WriteString("\n")
	}
	return strings.ReplaceAll(string(mustRead(repairTemplate)), "{{ISSUES}}", strings.TrimSuffix(b.String(), "\n"))
}

// Issue is one problem reported by the browser's validator. It mirrors
// `SceneError.issues` in src/scene/error.ts.
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// sanitizeLine collapses whitespace so one reported value cannot inject extra lines into the
// repair prompt. The content is still untrusted; this only keeps the format intact.
func sanitizeLine(s string) string {
	const maxLen = 300
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxLen {
		s = s[:maxLen] + "…"
	}
	return s
}
