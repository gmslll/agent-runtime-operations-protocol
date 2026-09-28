package runner

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"
)

type stringList []string

func (values *stringList) String() string { return fmt.Sprint([]string(*values)) }
func (values *stringList) Set(value string) error {
	if value == "" {
		return errors.New("scenario filter cannot be empty")
	}
	*values = append(*values, value)
	return nil
}

type ConformanceFailure struct{ Profile string }

func (failure ConformanceFailure) Error() string {
	return "conformance profile " + failure.Profile + " failed"
}

func ExecuteCLI(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	if stdout == nil || stderr == nil {
		return errors.New("conformance stdout and stderr are required")
	}
	flags := flag.NewFlagSet("arop-conformance", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", ".", "repository or conformance package root")
	profile := flags.String("profile", "", "profile ID")
	target := flags.String("target", "", "standalone conformance driver executable")
	jsonPath := flags.String("json", "", "JSON report path, or - for stdout")
	junitPath := flags.String("junit", "", "JUnit XML report path")
	timeout := flags.Duration("timeout", 0, "optional per-scenario timeout ceiling")
	var scenarios stringList
	flags.Var(&scenarios, "scenario", "scenario ID filter; repeatable")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *profile == "" || *target == "" {
		return errors.New("usage: --profile <id> --target <executable> [--root <dir>] [--scenario <id>] [--json <path|->] [--junit <path>]")
	}
	if *timeout < 0 || *timeout > time.Minute {
		return errors.New("timeout ceiling must be between 0 and 1m")
	}
	report, err := Run(ctx, Config{Root: *root, Profile: *profile, ScenarioFilters: scenarios, Target: *target, TimeoutCeiling: *timeout})
	if err != nil {
		return err
	}
	if err := WriteReports(report, *jsonPath, *junitPath); err != nil {
		return err
	}
	if *jsonPath == "" || *jsonPath == "-" {
		document, err := EncodeJSON(report)
		if err != nil {
			return err
		}
		if _, err := stdout.Write(append(document, '\n')); err != nil {
			return errors.New("write conformance report")
		}
	}
	if !report.Passed {
		return ConformanceFailure{Profile: report.Profile}
	}
	return nil
}
