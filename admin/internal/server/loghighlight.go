package server

import "regexp"

// Severity levels for log line highlighting.
type Severity string

const (
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

type logRule struct {
	pattern  *regexp.Regexp
	severity Severity
}

var logRules = []logRule{
	{regexp.MustCompile(`TLS Error`), SeverityError},
	{regexp.MustCompile(`AUTH_FAILED`), SeverityError},
	{regexp.MustCompile(`Connection reset`), SeverityError},
	{regexp.MustCompile(`Inactivity timeout`), SeverityWarn},
	{regexp.MustCompile(`Initialization Sequence Completed`), SeverityInfo},
	{regexp.MustCompile(`connection refused`), SeverityError},
	{regexp.MustCompile(`timeout`), SeverityWarn},
	{regexp.MustCompile(`EOF`), SeverityWarn},
}

// classifyLogLine returns the severity for a log line based on marker regexes.
func classifyLogLine(line string) Severity {
	for _, rule := range logRules {
		if rule.pattern.MatchString(line) {
			return rule.severity
		}
	}
	return SeverityInfo
}
