package core

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

var traceParentPattern = regexp.MustCompile(`^([0-9a-f]{2})-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})(.*)$`)

type TraceContext struct {
	Traceparent string `json:"traceparent"`
	Tracestate  string `json:"tracestate,omitempty"`
}

func ValidateTraceParent(value string) error {
	match := traceParentPattern.FindStringSubmatch(value)
	if match == nil {
		return fmt.Errorf("invalid traceparent shape")
	}
	if match[1] == "ff" {
		return fmt.Errorf("traceparent version ff is forbidden")
	}
	if match[2] == strings.Repeat("0", 32) {
		return fmt.Errorf("trace ID must not be all zero")
	}
	if match[3] == strings.Repeat("0", 16) {
		return fmt.Errorf("parent ID must not be all zero")
	}
	if match[1] == "00" && match[5] != "" {
		return fmt.Errorf("traceparent v00 must not contain extra fields")
	}
	if match[1] != "00" && match[5] != "" {
		if !strings.HasPrefix(match[5], "-") || len(match[5]) < 2 {
			return fmt.Errorf("invalid future-version traceparent extension")
		}
		for _, r := range match[5][1:] {
			if r < 0x21 || r > 0x7e {
				return fmt.Errorf("invalid future-version traceparent extension")
			}
		}
	}
	return nil
}

func (context TraceContext) Validate() error {
	if !utf8.ValidString(context.Tracestate) {
		return fmt.Errorf("tracestate is not valid UTF-8")
	}
	if utf8.RuneCountInString(context.Tracestate) > 512 {
		return fmt.Errorf("tracestate exceeds 512 Unicode code points")
	}
	return ValidateTraceParent(context.Traceparent)
}
