package server

import "testing"

func TestClassifyLogLine(t *testing.T) {
	tests := []struct {
		line string
		want Severity
	}{
		{"TLS Error: TLS handshake failed", SeverityError},
		{"TLS Error: TLS object -> invalid", SeverityError},
		{"AUTH_FAILED", SeverityError},
		{"Connection reset by peer", SeverityError},
		{"Inactivity timeout (--ping-restart)", SeverityWarn},
		{"Initialization Sequence Completed", SeverityInfo},
		{"connection refused", SeverityError},
		{"read tcp: connection refused", SeverityError},
		{"operation timeout", SeverityWarn},
		{"unexpected EOF", SeverityWarn},
		{"EOF", SeverityWarn},
		{"some random log line", SeverityInfo},
		{"", SeverityInfo},
		{"WARN: route config changed", SeverityInfo},
		{"TLS Error: new", SeverityError},
	}

	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			got := classifyLogLine(tt.line)
			if got != tt.want {
				t.Errorf("classifyLogLine(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

func TestClassifyLogLine_Priority(t *testing.T) {
	tests := []struct {
		name string
		line string
		want Severity
	}{
		{"error takes priority over warn", "TLS Error timeout", SeverityError},
		{"first matching rule wins", "Connection reset EOF", SeverityError},
		{"warn before info", "Inactivity timeout Completed", SeverityWarn},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyLogLine(tt.line)
			if got != tt.want {
				t.Errorf("classifyLogLine(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}
