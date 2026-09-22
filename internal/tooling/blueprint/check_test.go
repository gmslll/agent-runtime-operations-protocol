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

func TestNodeASTRejectsComputedAndReflectiveEscapes(t *testing.T) {
	t.Parallel()
	for name, source := range map[string]string{
		"aliased-process-getbuiltin":   `const p = process; p.getBuiltinModule("node:child_process")`,
		"async-function-constructor":   `Object.getPrototypeOf(async function(){}).constructor("return process")()`,
		"computed-child-process":       `process["get" + "BuiltinModule"]("node:child_process")`,
		"constructor-constructor":      `({}).constructor.constructor("return process")()`,
		"destructured-process-binding": `const { binding: b } = process; b("fs")`,
		"dynamic-import":               `import("node:http")`,
		"global-fetch":                 `await fetch("https://example.com")`,
		"global-object-fetch":          `await global.fetch("https://example.com")`,
		"global-process-getbuiltin":    `global.process.getBuiltinModule("node:child_process")`,
		"global-this-alias":            `const g = globalThis; await g.fetch("https://example.com")`,
		"global-reflect-get-binding":   `globalThis["Reflect"]["get"](process, "binding")("fs")`,
		"global-websocket":             `new WebSocket("wss://example.com")`,
		"process-env-secret":           `console.log(process.env.AROP_SECRET)`,
		"reflective-binding":           `Reflect.get(process, "binding")("fs")`,
		"unicode-constructor":          `({}).constr\u0075ctor.constr\u0075ctor("return process")()`,
		"unicode-fetch":                `await f\u0065tch("https://example.com")`,
		"unicode-global-this":          `globalTh\u0069s.process.getBuiltinModule("node:child_process")`,
		"unicode-process":              `pr\u006fcess.getBuiltinModule("node:child_process")`,
		"unicode-reflect":              `Refl\u0065ct.get(pr\u006fcess, "binding")("fs")`,
	} {
		t.Run(name, func(t *testing.T) {
			analysis, err := analyzeNodeSource(source)
			if err != nil {
				t.Fatal(err)
			}
			if len(analysis.Problems) == 0 {
				t.Fatalf("AST escape accepted; analysis=%+v", analysis)
			}
		})
	}
}

func TestCommandNormalizationRejectsWorkflowAliases(t *testing.T) {
	t.Parallel()
	source := "env:\n  NODE_BIN: node\njobs:\n  x:\n    steps:\n      - run: $NODE_BIN scripts/release/evil.mjs\n"
	if problems := commandSourceProblems("workflow", source, false, []Artifact{}); len(problems) == 0 {
		t.Fatal("workflow environment alias escaped normalization")
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
	entry := Artifact{ID: "allowed", Path: "scripts/allowed.mjs", Kind: "tooling", Status: "present", PathRole: "concrete", ImplementationRuntime: "node", ToolScope: "schema-validation"}
	for _, tc := range []struct {
		name        string
		makefile    string
		packageJSON string
		files       map[string]string
		artifacts   []Artifact
		symlink     string
		want        string
	}{
		{name: "extensionless env-S node", packageJSON: `{"scripts":{}}`, files: map[string]string{"tools/release-runner": "\ufeff  #!/usr/bin/env -S node\r\nconsole.log('x')\n"}, want: "not covered by an exact"},
		{name: "recursive package alias", packageJSON: `{"scripts":{"a":"npm run b","b":"npm run c","c":"node tools/evil"}}`, want: "unapproved exact Node entry"},
		{name: "cyclic package alias", packageJSON: `{"scripts":{"a":"npm run b","b":"npm run a"}}`, want: "alias cycle"},
		{name: "package runtime trampoline", packageJSON: `{"scripts":{"escape":"npm exec tsx tools/evil.ts"}}`, want: "non-allowlisted runtime"},
		{name: "make variable alias", makefile: "NODE_BIN:=node\nx:\n\t$(NODE_BIN) -e x\n", packageJSON: `{"scripts":{}}`, want: "non-allowlisted Node runtime"},
		{name: "make continuation", makefile: "x:\n\tnode \\\n\t  --permission --allow-fs-read=. --disable-proto=throw --no-addons tools/evil.mjs\n", packageJSON: `{"scripts":{}}`, want: "unapproved exact Node entry"},
		{name: "node options require", makefile: "x:\n\tNODE_OPTIONS=--require=./evil.cjs node --permission --allow-fs-read=. --disable-proto=throw --no-addons scripts/allowed.mjs\n", packageJSON: `{"scripts":{}}`, artifacts: []Artifact{entry}, want: "loader/search-path"},
		{name: "node options import", makefile: "x:\n\tNODE_OPTIONS=--import=./evil.mjs node --permission --allow-fs-read=. --disable-proto=throw --no-addons scripts/allowed.mjs\n", packageJSON: `{"scripts":{}}`, artifacts: []Artifact{entry}, want: "loader/search-path"},
		{name: "node path injection", makefile: "x:\n\tNODE_PATH=./evil node --permission --allow-fs-read=. --disable-proto=throw --no-addons scripts/allowed.mjs\n", packageJSON: `{"scripts":{}}`, artifacts: []Artifact{entry}, want: "loader/search-path"},
		{name: "node options append", makefile: "x:\n\tNODE_OPTIONS+=--require=./evil.cjs node --permission --allow-fs-read=. --disable-proto=throw --no-addons scripts/allowed.mjs\n", packageJSON: `{"scripts":{}}`, artifacts: []Artifact{entry}, want: "loader/search-path"},
		{name: "node path shadow", makefile: "x:\n\tPATH=/tmp/evil node --permission --allow-fs-read=. --disable-proto=throw --no-addons scripts/allowed.mjs\n", packageJSON: `{"scripts":{}}`, artifacts: []Artifact{entry}, want: "mutates PATH"},
		{name: "node alias shadow", makefile: "alias node=/tmp/evil\nx:\n\tnode --permission --allow-fs-read=. --disable-proto=throw --no-addons scripts/allowed.mjs\n", packageJSON: `{"scripts":{}}`, artifacts: []Artifact{entry}, want: "redefines or rebinds"},
		{name: "double quoted node", makefile: "x:\n\t\"node\" scripts/allowed.mjs\n", packageJSON: `{"scripts":{}}`, artifacts: []Artifact{entry}, want: "obfuscates the Node command"},
		{name: "single quoted node", makefile: "x:\n\t'node' scripts/allowed.mjs\n", packageJSON: `{"scripts":{}}`, artifacts: []Artifact{entry}, want: "obfuscates the Node command"},
		{name: "escaped node token", makefile: "x:\n\tno\\de scripts/allowed.mjs\n", packageJSON: `{"scripts":{}}`, artifacts: []Artifact{entry}, want: "obfuscates the Node command"},
		{name: "concatenated quoted node token", makefile: "x:\n\tn''ode scripts/allowed.mjs\n", packageJSON: `{"scripts":{}}`, artifacts: []Artifact{entry}, want: "obfuscates the Node command"},
		{name: "sudo node wrapper", makefile: "x:\n\tsudo node --permission --allow-fs-read=. --disable-proto=throw --no-addons scripts/allowed.mjs\n", packageJSON: `{"scripts":{}}`, artifacts: []Artifact{entry}, want: "non-exact shell/environment"},
		{name: "nice node wrapper", makefile: "x:\n\tnice node --permission --allow-fs-read=. --disable-proto=throw --no-addons scripts/allowed.mjs\n", packageJSON: `{"scripts":{}}`, artifacts: []Artifact{entry}, want: "non-exact shell/environment"},
		{name: "time node wrapper", makefile: "x:\n\ttime node --permission --allow-fs-read=. --disable-proto=throw --no-addons scripts/allowed.mjs\n", packageJSON: `{"scripts":{}}`, artifacts: []Artifact{entry}, want: "non-exact shell/environment"},
		{name: "absolute node binary", makefile: "x:\n\t/usr/bin/node --permission --allow-fs-read=. --disable-proto=throw --no-addons scripts/allowed.mjs\n", packageJSON: `{"scripts":{}}`, artifacts: []Artifact{entry}, want: "non-exact filesystem path"},
		{name: "root node binary", makefile: "x:\n\t/node --permission --allow-fs-read=. --disable-proto=throw --no-addons scripts/allowed.mjs\n", packageJSON: `{"scripts":{}}`, artifacts: []Artifact{entry}, want: "non-exact filesystem path"},
		{name: "shell resolved node binary", makefile: "x:\n\t$$(which node) --permission --allow-fs-read=. --disable-proto=throw --no-addons scripts/allowed.mjs\n", packageJSON: `{"scripts":{}}`, artifacts: []Artifact{entry}, want: "shell substitution"},
		{name: "shell parameter node binary", makefile: "x:\n\t$${NODE_RUNTIME:-node} --permission --allow-fs-read=. --disable-proto=throw --no-addons scripts/allowed.mjs\n", packageJSON: `{"scripts":{}}`, artifacts: []Artifact{entry}, want: "variable expansion"},
		{name: "env wrapped node binary", makefile: "x:\n\tenv node --permission --allow-fs-read=. --disable-proto=throw --no-addons scripts/allowed.mjs\n", packageJSON: `{"scripts":{}}`, artifacts: []Artifact{entry}, want: "non-exact shell/environment"},
		{name: "workflow env alias", packageJSON: `{"scripts":{}}`, files: map[string]string{".github/workflows/evil.yml": "env:\n  NODE_BIN: node\njobs:\n  x:\n    steps:\n      - run: $NODE_BIN scripts/release/evil.mjs\n"}, want: "invokes"},
		{name: "production Containerfile", packageJSON: `{"scripts":{}}`, files: map[string]string{"deploy/Containerfile.prod": "FROM scratch\nRUN node tools/evil.js\n"}, want: "production image contains Node runtime/tooling"},
		{name: "allowed tool undeclared helper", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "import './helper.mjs'\n", "scripts/helper.mjs": "export const ok = true\n"}, artifacts: []Artifact{entry, {ID: "helper", Path: "scripts/helper.mjs", Kind: "tooling-helper", Status: "present", PathRole: "concrete", ImplementationRuntime: "node", ToolScope: "schema-validation"}}, want: "without manifest derives_from"},
		{name: "allowlisted tool child-process escape", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "import { execSync } from 'node:child_process';\nexecSync('git status');\n"}, artifacts: []Artifact{entry}, want: "forbidden Node builtin"},
		{name: "dynamic import", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "await import(process.argv[2])\n"}, artifacts: []Artifact{entry}, want: "uses dynamic import"},
		{name: "commonjs require", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "require('node:fs')\n"}, artifacts: []Artifact{entry}, want: "require"},
		{name: "create require", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "createRequire(import.meta.url)\n"}, artifacts: []Artifact{entry}, want: "createRequire"},
		{name: "eval", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "eval('1')\n"}, artifacts: []Artifact{entry}, want: "eval"},
		{name: "function constructor", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "new Function('return 1')\n"}, artifacts: []Artifact{entry}, want: "Function"},
		{name: "process binding", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "process.binding('fs')\n"}, artifacts: []Artifact{entry}, want: "binding"},
		{name: "process dlopen", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "process.dlopen(module, './x.node')\n"}, artifacts: []Artifact{entry}, want: "dlopen"},
		{name: "process get builtin", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "process.getBuiltinModule('node:fs')\n"}, artifacts: []Artifact{entry}, want: "getBuiltinModule"},
		{name: "computed getBuiltinModule child-process", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "process['get' + 'BuiltinModule']('node:child_process')\n"}, artifacts: []Artifact{entry}, want: "dangerous computed"},
		{name: "node http builtin", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "import http from 'node:http'\n"}, artifacts: []Artifact{entry}, want: "forbidden Node builtin"},
		{name: "node net builtin", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "import net from 'node:net'\n"}, artifacts: []Artifact{entry}, want: "forbidden Node builtin"},
		{name: "node tls builtin", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "import tls from 'node:tls'\n"}, artifacts: []Artifact{entry}, want: "forbidden Node builtin"},
		{name: "node dgram builtin", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "import dgram from 'node:dgram'\n"}, artifacts: []Artifact{entry}, want: "forbidden Node builtin"},
		{name: "node cluster builtin", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "import cluster from 'node:cluster'\n"}, artifacts: []Artifact{entry}, want: "forbidden Node builtin"},
		{name: "node worker builtin", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "import { Worker } from 'node:worker_threads'\n"}, artifacts: []Artifact{entry}, want: "forbidden Node builtin"},
		{name: "computed global require", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "globalThis['requ' + 'ire']('node:fs')\n"}, artifacts: []Artifact{entry}, want: "dangerous computed"},
		{name: "vm builtin", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "import vm from 'node:vm'\n"}, artifacts: []Artifact{entry}, want: "forbidden Node builtin"},
		{name: "data import", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "import 'data:text/javascript,export default 1'\n"}, artifacts: []Artifact{entry}, want: "data/file imports are forbidden"},
		{name: "file import", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "import 'file:///tmp/escape.mjs'\n"}, artifacts: []Artifact{entry}, want: "data/file imports are forbidden"},
		{name: "absolute import", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "import '/tmp/escape.mjs'\n"}, artifacts: []Artifact{entry}, want: "absolute imports are forbidden"},
		{name: "undeclared bare import", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "import x from 'not-declared'\n"}, artifacts: []Artifact{entry}, want: "undeclared bare import"},
		{name: "reflection escape", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed.mjs": "Reflect.get(process, 'binding')('fs')\n"}, artifacts: []Artifact{entry}, want: "dangerous reflection"},
		{name: "native addon import", packageJSON: `{"scripts":{},"dependencies":{"bindings":"1.5.0"}}`, files: map[string]string{"scripts/allowed.mjs": "import bindings from 'bindings'\n"}, artifacts: []Artifact{entry}, want: "native addon imports are forbidden"},
		{name: "registered unsafe node shebang", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed": "#!/usr/bin/env -S node --import=./evil.mjs\n"}, artifacts: []Artifact{{ID: "allowed-shebang", Path: "scripts/allowed", Kind: "tooling", Status: "present", PathRole: "concrete", ImplementationRuntime: "node", ToolScope: "schema-validation"}}, want: "Node shebang execution is forbidden"},
		{name: "registered quoted node shebang", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed": "#!/usr/bin/env -S 'node --import=./evil.mjs'\n"}, artifacts: []Artifact{{ID: "allowed-shebang", Path: "scripts/allowed", Kind: "tooling", Status: "present", PathRole: "concrete", ImplementationRuntime: "node", ToolScope: "schema-validation"}}, want: "Node shebang execution is forbidden"},
		{name: "registered fragmented node shebang", packageJSON: `{"scripts":{}}`, files: map[string]string{"scripts/allowed": "#!/usr/bin/env -S n\"\"ode\n"}, artifacts: []Artifact{{ID: "allowed-shebang", Path: "scripts/allowed", Kind: "tooling", Status: "present", PathRole: "concrete", ImplementationRuntime: "node", ToolScope: "schema-validation"}}, want: "Node shebang execution is forbidden"},
		{name: "worktrees executable node", packageJSON: `{"scripts":{}}`, files: map[string]string{".worktrees/evil.mjs": "await fetch('https://example.com')\n"}, want: "not covered by an exact"},
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

func TestExactSafeNodePackageTemplatePasses(t *testing.T) {
	t.Parallel()
	entry := Artifact{ID: "allowed", Path: "scripts/allowed.mjs", Kind: "tooling", Status: "present", PathRole: "concrete", ImplementationRuntime: "node", ToolScope: "schema-validation"}
	scripts := map[string]any{
		"entry": "node --permission --allow-fs-read=. --disable-proto=throw --no-addons scripts/allowed.mjs",
		"alias": "npm run entry",
	}
	if problems := packageScriptProblems(scripts, []Artifact{entry}); len(problems) != 0 {
		t.Fatalf("exact safe Node package template rejected: %v", problems)
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
