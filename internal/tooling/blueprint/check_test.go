package blueprint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBlueprintBaselineAndEmbeddedNegativeProbes(t *testing.T) {
	t.Parallel()
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	checks, _, _ := Run(root)
	for _, check := range checks {
		if !check.Passed {
			t.Errorf("%s: %s", check.Name, check.Detail)
		}
	}
}

func TestBoundaryNegativeProbesAreIndividuallyRejected(t *testing.T) {
	t.Parallel()
	if escaped := boundaryNegativeProbes(); len(escaped) != 0 {
		for _, issue := range escaped {
			t.Error(issue)
		}
	}
}

func TestPositivePublicV01ProbeIsRejected(t *testing.T) {
	t.Parallel()
	problems := publicV01LineProblems("Milestone: publish public v0.1 before v1 RC.", "probe")
	if len(problems) != 1 {
		t.Fatalf("expected one violation, got %v", problems)
	}
}

func TestRuntimeBoundaryRejectsRealFilesystemEscapes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		makefile    string
		packageJSON string
		files       map[string]string
		artifacts   []Artifact
		symlink     string
		want        string
	}{
		{name: "extensionless env-S node", packageJSON: `{"scripts":{}}`, files: map[string]string{"tools/release-runner": "\ufeff  #!/usr/bin/env -S node\r\nconsole.log('x')\n"}, want: "executable Node file is not covered"},
		{name: "recursive package alias", packageJSON: `{"scripts":{"a":"npm run b","b":"npm run c","c":"node tools/evil"}}`, want: "unapproved Node target"},
		{name: "cyclic package alias", packageJSON: `{"scripts":{"a":"npm run b","b":"npm run a"}}`, want: "alias cycle"},
		{name: "package runtime trampoline", packageJSON: `{"scripts":{"escape":"npm exec tsx tools/evil.ts"}}`, want: "non-allowlisted runtime"},
		{name: "make variable alias", makefile: "NODE_BIN:=node\nx:\n\t$(NODE_BIN) -e x\n", packageJSON: `{"scripts":{}}`, want: "non-allowlisted Node runtime"},
		{name: "workflow env alias", packageJSON: `{"scripts":{}}`, files: map[string]string{".github/workflows/evil.yml": "env:\n  NODE_BIN: node\njobs:\n  x:\n    steps:\n      - run: $NODE_BIN scripts/release/evil.mjs\n"}, want: "invokes"},
		{name: "production Containerfile", packageJSON: `{"scripts":{}}`, files: map[string]string{"deploy/Containerfile.prod": "FROM scratch\nRUN node tools/evil.js\n"}, want: "production image contains Node runtime/tooling"},
		{name: "allowed tool undeclared helper", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "import './helper.mjs'\n", "scripts/helper.mjs": "export const ok = true\n"}, artifacts: []Artifact{{ID: "allowed", Path: "scripts/allowed.mjs", Kind: "tooling", Status: "present", PathRole: "concrete", ImplementationRuntime: "node", ToolScope: "schema-validation"}, {ID: "helper", Path: "scripts/helper.mjs", Kind: "tooling", Status: "present", PathRole: "concrete", ImplementationRuntime: "node", ToolScope: "schema-validation"}}, want: "without manifest derives_from"},
		{name: "allowlisted tool child-process escape", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "import { execSync } from 'node:child_process';\nexecSync('git status');\n"}, artifacts: []Artifact{{ID: "allowed", Path: "scripts/allowed.mjs", Kind: "tooling", Status: "present", PathRole: "concrete", ImplementationRuntime: "node", ToolScope: "schema-validation"}}, want: "imports child_process"},
		{name: "allowlisted tool dynamic execution escape", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "const moduleName = process.argv[2];\nawait import(moduleName);\neval('1');\nnew Function('return 1')();\nrequire(moduleName);\n"}, artifacts: []Artifact{{ID: "allowed", Path: "scripts/allowed.mjs", Kind: "tooling", Status: "present", PathRole: "concrete", ImplementationRuntime: "node", ToolScope: "schema-validation"}}, want: "uses dynamic import"},
		{name: "external symlink escape", packageJSON: `{"scripts":{}}`, symlink: "external", want: "symlink escapes repository"},
		{name: "internal unregistered symlink", packageJSON: `{"scripts":{}}`, symlink: "unregistered", want: "symlink enters an unregistered artifact"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if tc.packageJSON == "" {
				tc.packageJSON = `{"scripts":{}}`
			}
			if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(tc.packageJSON), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(tc.makefile), 0o644); err != nil {
				t.Fatal(err)
			}
			for path, content := range tc.files {
				absolute := filepath.Join(root, filepath.FromSlash(path))
				if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(absolute, []byte(content), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tc.symlink == "external" {
				outside := filepath.Join(t.TempDir(), "outside.mjs")
				if err := os.WriteFile(outside, []byte("#!/usr/bin/env node\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
					t.Fatal(err)
				}
			} else if tc.symlink == "unregistered" {
				target := filepath.Join(root, "hidden.txt")
				if err := os.WriteFile(target, []byte("hidden\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Base(target), filepath.Join(root, "alias")); err != nil {
					t.Fatal(err)
				}
			}
			problems := runtimeBoundary(root, tc.artifacts, indexArtifacts(tc.artifacts), tc.makefile, tc.packageJSON)
			if len(problems) == 0 {
				t.Fatal("filesystem escape was accepted")
			}
			if detail := strings.Join(problems, "\n"); !strings.Contains(detail, tc.want) {
				t.Fatalf("wanted %q, got:\n%s", tc.want, detail)
			}
		})
	}
}

func TestMarkdownLinkClosureRejectsEmptyTargetWithoutPanicking(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("[empty]()\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	problems := localLinks(root)
	if len(problems) != 1 || !strings.Contains(problems[0], "empty local link") {
		t.Fatalf("empty link was not rejected precisely: %v", problems)
	}
}
