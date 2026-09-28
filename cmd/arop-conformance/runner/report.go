package runner

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type junitSuite struct {
	XMLName    xml.Name        `xml:"testsuite"`
	Name       string          `xml:"name,attr"`
	Tests      int             `xml:"tests,attr"`
	Failures   int             `xml:"failures,attr"`
	Skipped    int             `xml:"skipped,attr"`
	Properties []junitProperty `xml:"properties>property"`
	Cases      []junitCase     `xml:"testcase"`
}

type junitProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type junitCase struct {
	ClassName string        `xml:"classname,attr"`
	Name      string        `xml:"name,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	Skipped   *junitSkipped `xml:"skipped,omitempty"`
	SystemOut string        `xml:"system-out"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
}

type junitSkipped struct {
	Message string `xml:"message,attr"`
}

func EncodeJSON(report Report) ([]byte, error) {
	return json.MarshalIndent(report, "", "  ")
}

func EncodeJUnit(report Report) ([]byte, error) {
	suite := junitSuite{Name: "AROP portable conformance " + report.Profile, Tests: len(report.Results), Failures: report.FailedCount, Skipped: report.SkippedCount, Properties: []junitProperty{{Name: "driver_protocol", Value: report.Protocol}, {Name: "profile", Value: report.Profile}, {Name: "profile_sha256", Value: report.ProfileSHA256}, {Name: "target_sha256", Value: report.TargetSHA256}}}
	for _, result := range report.Results {
		evidence, err := json.Marshal(map[string]any{"required": result.Required, "scenario_sha256": result.ScenarioSHA256, "fixture_path": result.FixturePath, "fixture_sha256": result.FixtureSHA256, "fixture_bytes": result.FixtureBytes})
		if err != nil {
			return nil, err
		}
		item := junitCase{ClassName: "arop.conformance." + report.Profile, Name: result.ID, Time: strconv.FormatFloat(float64(result.DurationMS)/1000, 'f', 3, 64), SystemOut: string(evidence)}
		if result.Outcome == "fail" {
			item.Failure = &junitFailure{Message: result.Message}
		} else if result.Outcome == "skip" {
			item.Skipped = &junitSkipped{Message: result.Message}
		}
		suite.Cases = append(suite.Cases, item)
	}
	document, err := xml.MarshalIndent(suite, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), append(document, '\n')...), nil
}

func WriteReports(report Report, jsonPath, junitPath string) error {
	if jsonPath != "" && jsonPath != "-" {
		document, err := EncodeJSON(report)
		if err != nil {
			return err
		}
		if err := writeAtomic(jsonPath, append(document, '\n')); err != nil {
			return err
		}
	}
	if junitPath != "" {
		document, err := EncodeJUnit(report)
		if err != nil {
			return err
		}
		if err := writeAtomic(junitPath, document); err != nil {
			return err
		}
	}
	return nil
}

func writeAtomic(name string, data []byte) error {
	if name == "" || name == "-" {
		return errors.New("report output path is invalid")
	}
	name, err := filepath.Abs(name)
	if err != nil {
		return errors.New("report output path is invalid")
	}
	parent := filepath.Dir(name)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return errors.New("create report directory")
	}
	if info, err := os.Lstat(name); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("report output symlink rejected")
	} else if err != nil && !os.IsNotExist(err) {
		return errors.New("inspect report output")
	}
	temporary, err := os.CreateTemp(parent, ".arop-report-*")
	if err != nil {
		return errors.New("create report output")
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return errors.New("secure report output")
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return errors.New("write report output")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return errors.New("sync report output")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("close report output")
	}
	if err := os.Rename(temporaryName, name); err != nil {
		return fmt.Errorf("publish report output: %w", err)
	}
	return nil
}

func reportDuration(report Report) time.Duration {
	started, _ := time.Parse(time.RFC3339Nano, report.StartedAt)
	completed, _ := time.Parse(time.RFC3339Nano, report.CompletedAt)
	return completed.Sub(started)
}
