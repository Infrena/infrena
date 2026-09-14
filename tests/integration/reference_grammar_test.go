package integration

import (
	"strings"
	"testing"
)

// TestVarPathsAndResourceAttributePathsPlanEndToEnd is Task 7's own coverage:
// the reference-grammar rewrite (${var.x}, and a path after either a
// variable or a resource attribute) exercised through a real `plan`, not just
// the parser's unit tests. Three shapes in one project, because each is a
// different consumer of the same path machinery and a fixture that only
// covered one could pass while another regressed.
func TestVarPathsAndResourceAttributePathsPlanEndToEnd(t *testing.T) {
	dir := project(t, `
project: PathDemo
environments:
  dev: {}
variables:
  tags:
    type: map
    default:
      team: payments
      owner: platform
  azs:
    type: list
    default:
      - us-east-1a
      - us-east-1b
      - us-east-1c
resources:
  net:
    type: fake.network
    cidr: ${var.azs[1]}
  db:
    type: fake.database
    engine: ${var.tags.team}
    network: ${net.id}
    tags:
      owner: ${var.tags.owner}
  app:
    type: fake.application
    image: myapp:latest
    database_url: ${db.tags.owner}
`)

	r := run(t, dir, "plan", "dev")
	if r.ExitCode != 2 {
		t.Fatalf("plan exit = %d, want 2:\n%s", r.ExitCode, r.combined())
	}

	// A LIST INDEX into a variable: ${var.azs[1]} picks the second element.
	if line := attrLine(t, r.Stdout, "fake.network.net", "cidr"); !strings.HasPrefix(line, `cidr: "us-east-1b"`) {
		t.Errorf("net.cidr = %q, want it to start with %q — ${var.azs[1]} did not resolve to the "+
			"list's second element", line, `cidr: "us-east-1b"`)
	}

	// A MAP PATH into a variable: ${var.tags.team} picks one key.
	if line := attrLine(t, r.Stdout, "fake.database.db", "engine"); !strings.HasPrefix(line, `engine: "payments"`) {
		t.Errorf("db.engine = %q, want it to start with %q — ${var.tags.team} did not resolve to "+
			"the map's team key", line, `engine: "payments"`)
	}

	// A PATH INTO A RESOURCE ATTRIBUTE: ${db.tags.owner} reaches through a
	// map attribute set by ANOTHER path expression (${var.tags.owner}), so
	// this also pins that a path can consume a path's own output.
	if line := attrLine(t, r.Stdout, "fake.application.app", "database_url"); line != `database_url: "platform"` {
		t.Errorf("app.database_url = %q, want %q — ${db.tags.owner} did not resolve", line,
			`database_url: "platform"`)
	}
}
