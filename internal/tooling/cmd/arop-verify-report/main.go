package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/report"
	"github.com/gmslll/agent-runtime-operations-protocol/internal/tooling/structuredfile"
)

func main() {
	allow := flag.Bool("allow-ancestor", os.Getenv("ALLOW_ANCESTOR") == "1", "allow verified ancestor report")
	flag.Parse()
	path := os.Getenv("REPORT")
	if path == "" && flag.NArg() > 0 {
		path = flag.Arg(0)
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, "usage: arop-verify-report [--allow-ancestor] <report.json>")
		os.Exit(2)
	}
	root, _ := structuredfile.FindRoot(".")
	r, mode, err := report.Verify(report.VerifyOptions{Root: root, ReportPath: path, AllowAncestor: *allow})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	current := git(root, "rev-parse", "HEAD")
	fmt.Printf("AROP report verified: %s; mode=%s; claimed=%s; current=%s; %d JSON/JUnit testcases, %v failures, lineage and digests match.\n", filepath.ToSlash(path), mode, r.Provenance.Git.Head, current, len(r.Checks), r.Summary["failed"])
}
func git(root string, args ...string) string {
	c := exec.Command("git", args...)
	c.Dir = root
	out, _ := c.Output()
	return strings.TrimSpace(string(out))
}
