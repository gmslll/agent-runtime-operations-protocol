package testinventory

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestInvocationNeutralizesSelectionAndCacheEnvironment(t *testing.T) {
	t.Parallel()
	inventory := Inventory{Packages: []Package{{Path: "./internal/tooling/example"}}}
	args, env := Invocation(inventory, []string{
		"KEEP=value",
		"GOFLAGS=-run=TestOnly -count=99",
		"GOENV=/tmp/host-goenv",
		"GOCACHE=/tmp/shared-cache",
		"GOCACHEPROG=/tmp/cache-helper",
		"GODEBUG=gocachetest=1",
		"GOTMPDIR=/tmp/shared-tmp",
		"GOWORK=/tmp/host.work",
		"AROP_VERIFY_CURRENT=0",
		"NODE_OPTIONS=--require=/tmp/escape.cjs",
		"NODE_PATH=/tmp/escape-modules",
		"NPM_CONFIG_NODE_OPTIONS=--import=/tmp/escape.mjs",
		"GIT_INDEX_FILE=/tmp/alternate-index",
	}, "/tmp/fresh-cache", "/tmp/fresh-tmp")
	wantArgs := "test -count=1 -run=. -json ./internal/tooling/example"
	if got := strings.Join(args, " "); got != wantArgs {
		t.Fatalf("fixed go test argv changed: %q", got)
	}
	values := envMap(env)
	for key, want := range map[string]string{
		"KEEP":                "value",
		"AROP_VERIFY_CURRENT": "1",
		"GOFLAGS":             "",
		"GOENV":               "off",
		"GOCACHE":             "/tmp/fresh-cache",
		"GODEBUG":             "",
		"GOTMPDIR":            "/tmp/fresh-tmp",
		"GOWORK":              "off",
	} {
		if values[key] != want {
			t.Errorf("%s=%q, want %q", key, values[key], want)
		}
	}
	for _, key := range []string{"GOCACHEPROG", "NODE_OPTIONS", "NODE_PATH", "NPM_CONFIG_NODE_OPTIONS", "GIT_INDEX_FILE"} {
		if _, exists := values[key]; exists {
			t.Fatalf("%s was not cleared", key)
		}
	}
}

func TestEvaluateRejectsInventoryBypasses(t *testing.T) {
	t.Parallel()
	const pkg = "example.invalid/internal/tooling/example"
	inventory := Inventory{Packages: []Package{{Package: pkg, Tests: []string{"TestExpected"}}}}
	passingTest := event("run", pkg, "TestExpected", "") + event("pass", pkg, "TestExpected", "")
	passingPackage := event("pass", pkg, "", "")
	cases := []struct {
		name       string
		log        string
		commandErr error
		want       string
	}{
		{name: "GOFLAGS=-run", log: passingPackage, want: "go-test-terminal-set"},
		{name: "TestMain_no-tests", log: event("output", pkg, "", "TestMain returned without m.Run\n") + passingPackage, want: "no testcase terminal events"},
		{name: "no-test-files", log: event("output", pkg, "", "?\texample.invalid\t[no test files]\n") + passingPackage, want: "go-test-no-empty-package"},
		{name: "cache", log: passingTest + event("output", pkg, "", "ok\texample.invalid\t(cached)\n") + passingPackage, want: "go-test-no-cache"},
		{name: "missing-test", log: passingPackage, want: "missing terminal event"},
		{name: "extra-test", log: passingTest + event("pass", pkg, "TestUnexpected", "") + passingPackage, want: "extra=["},
		{name: "failed-test", log: event("fail", pkg, "TestExpected", "") + event("fail", pkg, "", ""), commandErr: errors.New("exit status 1"), want: "go-test-no-fail"},
		{name: "skipped-test", log: event("skip", pkg, "TestExpected", "") + passingPackage, want: "go-test-no-skip"},
		{name: "missing-package", log: passingTest, want: "go-package-terminal-set"},
		{name: "extra-package", log: passingTest + passingPackage + event("pass", "example.invalid/extra", "", ""), want: "extra=[example.invalid/extra]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Evaluate(inventory, []byte(tc.log), tc.commandErr)
			if err != nil {
				t.Fatal(err)
			}
			if got.Passed {
				t.Fatal("bypass was accepted")
			}
			if detail := strings.Join(got.Details, "\n"); !strings.Contains(detail, tc.want) {
				t.Fatalf("wanted %q in diagnostics:\n%s", tc.want, detail)
			}
		})
	}
}

func event(action, pkg, test, output string) string {
	fields := map[string]string{"Action": action, "Package": pkg}
	if test != "" {
		fields["Test"] = test
	}
	if output != "" {
		fields["Output"] = output
	}
	data, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(data) + "\n"
}

func envMap(env []string) map[string]string {
	result := map[string]string{}
	for _, item := range env {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}
	return result
}
