package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemplate(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestATemplateIsInterpolatedInTheSameGrammar, including a RESOURCE reference,
// which is the case the feature exists for: an IAM policy naming a bucket.
//
// The reference resolving to a real value is also proof the dependency edge
// works — net.id is only knowable after the network is created, so a rendered
// "net-1" means the template waited for it.
func TestATemplateIsInterpolatedInTheSameGrammar(t *testing.T) {
	dir := project(t, `
project: myapp
variables:
  team:
    type: string
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${net}
    tags:
      policy: ${template.policy.json}
`)
	writeIn(t, dir, "variables.yml", "team: platform\n")
	writeTemplate(t, dir, "templates/policy.json",
		`{"Resource":"${net.id}","Team":"${var.team}"}`)

	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply: %d\n%s", a.ExitCode, a.combined())
	}
	state := readFileString(t, filepath.Join(dir, ".infrena", "state", "dev.json"))
	if !strings.Contains(state, `net-1`) || !strings.Contains(state, "platform") {
		t.Errorf("the template was not interpolated:\n%s", state)
	}
}

// TestAFileReferenceIsVerbatim is the reason ${file.} exists at all.
//
// Shell scripts and user-data contain ${...} of their own. Interpolating one
// would silently eat ${HOME} and substitute nothing, producing a script that
// RUNS and misbehaves rather than one that fails — which is the worse of the
// two outcomes by a distance.
func TestAFileReferenceIsVerbatim(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${net}
    tags:
      bootstrap: ${file.user-data.sh}
`)
	writeTemplate(t, dir, "templates/user-data.sh",
		"#!/bin/sh\necho \"${HOME} ${AWS_REGION} ${var.nope}\"\n")

	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply: %d\n%s", a.ExitCode, a.combined())
	}
	state := readFileString(t, filepath.Join(dir, ".infrena", "state", "dev.json"))
	for _, want := range []string{"${HOME}", "${AWS_REGION}", "${var.nope}"} {
		if !strings.Contains(state, strings.ReplaceAll(want, `"`, `\"`)) {
			t.Errorf("%s did not survive verbatim:\n%s", want, state)
		}
	}
}

// TestASecretInATemplateIsRedacted is the property that decided the design.
//
// A real template engine writes values with fmt.Fprint, so a secret embedded in
// a rendered policy becomes a plain string and flows into the plan, the state
// and the report in clear. Evaluating a template through the same evaluator
// configuration uses means sensitivity propagates through concatenation
// automatically: part of the document is secret, so the document is.
func TestASecretInATemplateIsRedacted(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${net}
    tags:
      creds: ${template.creds.json}
`)
	writeTemplate(t, dir, "templates/creds.json", `{"password":"${secret.DB_PASSWORD}"}`)
	t.Setenv("DB_PASSWORD", "hunter2")

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan: %d\n%s", r.ExitCode, r.combined())
	}
	if strings.Contains(r.combined(), "hunter2") {
		t.Errorf("a secret rendered through a template was printed in clear:\n%s", r.combined())
	}
	if !strings.Contains(r.combined(), "<sensitive>") {
		t.Errorf("the rendered document was not treated as sensitive:\n%s", r.combined())
	}
}

// TestAScopedTemplateBeatsTheProjectOne. Nearest wins, the same direction a
// directory's vars/ beats the project's.
func TestAScopedTemplateBeatsTheProjectOne(t *testing.T) {
	dir := project(t, `
project: myapp
`)
	writeIn(t, dir, "resources/databases/db.yml", `
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${net}
    tags:
      policy: ${template.policy.json}
`)
	writeTemplate(t, dir, "templates/policy.json", `{"from":"project"}`)
	writeTemplate(t, dir, "resources/databases/templates/policy.json", `{"from":"directory"}`)

	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply: %d\n%s", a.ExitCode, a.combined())
	}
	state := readFileString(t, filepath.Join(dir, ".infrena", "state", "dev.json"))
	if !strings.Contains(state, "directory") {
		t.Errorf("the project-wide template won; the directory's must:\n%s", state)
	}
}

// TestAMissingTemplateSaysWhereItLooked.
func TestAMissingTemplateSaysWhereItLooked(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: ${template.nope.json}
`)
	r := run(t, dir, "plan", "dev")
	if r.ExitCode == 2 {
		t.Fatal("a missing template produced a plan")
	}
	for _, want := range []string{"nope.json", "templates/"} {
		if !strings.Contains(r.combined(), want) {
			t.Errorf("the diagnostic never mentions %q:\n%s", want, r.combined())
		}
	}
}

// TestATemplateCycleIsRefused rather than expanded until the process dies.
func TestATemplateCycleIsRefused(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: ${template.a.txt}
`)
	writeTemplate(t, dir, "templates/a.txt", "${template.b.txt}")
	writeTemplate(t, dir, "templates/b.txt", "${template.a.txt}")

	r := run(t, dir, "plan", "dev")
	if r.ExitCode == 2 {
		t.Fatal("a template cycle produced a plan")
	}
	if !strings.Contains(r.combined(), "cycle") {
		t.Errorf("the diagnostic does not name the likely cause:\n%s", r.combined())
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

// TestATemplateTakesArguments, and the two passes cooperate: {{ }} loops over
// what was passed, and ${ } resolves a resource reference that survived it.
//
// The `net-1` in the output is the proof of the second half — it is only
// knowable after the network exists, so the reference kept its deferral and its
// dependency edge through the render.
func TestATemplateTakesArguments(t *testing.T) {
	dir := project(t, `
project: myapp
variables:
  args:
    type: map
    default:
      team: platform
      buckets: [logs, backups]
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${net}
    tags:
      policy: "${template.policy.json(var.args)}"
`)
	writeTemplate(t, dir, "templates/policy.json",
		`{"team":"{{ .team }}"{{ range $b := .buckets }},{"r":{{ $b | quote }},"vpc":"${net.id}"}{{ end }}}`)

	if a := run(t, dir, "apply", "dev", "--auto-approve"); a.ExitCode != 2 {
		t.Fatalf("apply: %d\n%s", a.ExitCode, a.combined())
	}
	state := readFileString(t, filepath.Join(dir, ".infrena", "state", "dev.json"))
	for _, want := range []string{"platform", "logs", "backups", "net-1"} {
		if !strings.Contains(state, want) {
			t.Errorf("the rendered document is missing %q:\n%s", want, state)
		}
	}
}

// TestASensitiveArgumentTaintsTheRenderedDocument.
//
// The template decides where the value lands, so there is no leaf to mark and
// the whole output is treated as secret. Coarse, and correct in the only
// direction that matters: the alternative is a rendered document carrying a
// credential in clear into the plan, the state and the report.
//
// This is the rule that makes the two-pass design safe. Pass one never sees the
// project's scope, so the only way a secret reaches it is deliberately, through
// an argument — where we evaluated it and still know what it was.
func TestASensitiveArgumentTaintsTheRenderedDocument(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: 10.0.0.0/16
  db:
    type: fake.database
    engine: postgres
    network: ${net}
    tags:
      creds: "${template.creds.json({user: app, password: ${secret.DB_PASSWORD}})}"
`)
	writeTemplate(t, dir, "templates/creds.json", `{"u":"{{ .user }}","p":"{{ .password }}"}`)
	t.Setenv("DB_PASSWORD", "hunter2")

	r := run(t, dir, "plan", "dev")
	if strings.Contains(r.combined(), "hunter2") {
		t.Errorf("a secret passed to a template was printed in clear:\n%s", r.combined())
	}
	if !strings.Contains(r.combined(), "<sensitive>") {
		t.Errorf("the rendered document was not treated as sensitive:\n%s", r.combined())
	}
}

// TestFileTakesNoArguments. It reads the file verbatim, so an argument would
// advertise a behaviour that does not happen.
func TestFileTakesNoArguments(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: "${file.thing.txt({a: b})}"
`)
	writeTemplate(t, dir, "templates/thing.txt", "plain")

	r := run(t, dir, "plan", "dev")
	if r.ExitCode == 2 {
		t.Fatal("file with arguments was accepted")
	}
	if !strings.Contains(r.combined(), "takes no arguments") {
		t.Errorf("the refusal does not explain itself:\n%s", r.combined())
	}
}

// TestATemplateErrorNamesTheFileAndItsFunctions. A {{ }} mistake is reported
// against the template, not the configuration line, and lists what is available
// — because the function set is ours and small, so a reader cannot be expected
// to know it from elsewhere.
func TestATemplateErrorNamesTheFileAndItsFunctions(t *testing.T) {
	dir := project(t, `
project: myapp
resources:
  net:
    type: fake.network
    cidr: "${template.broken.txt({a: b})}"
`)
	writeTemplate(t, dir, "templates/broken.txt", "{{ nosuchfunc .a }}")

	r := run(t, dir, "plan", "dev")
	if r.ExitCode == 2 {
		t.Fatal("a broken template rendered")
	}
	out := r.combined()
	for _, want := range []string{"broken.txt", "until", "quote"} {
		if !strings.Contains(out, want) {
			t.Errorf("the diagnostic never mentions %q:\n%s", want, out)
		}
	}
}
