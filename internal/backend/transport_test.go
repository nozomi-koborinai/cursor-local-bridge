package backend

import "testing"

func TestIsTransportError(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{text: "RetriableError: http/2 stream closed", want: true},
		{text: "  RetriableError: CANCEL", want: true},
		{text: "RetriableError", want: true},
		{text: "prefix RetriableError: http/2 stream closed", want: true},
		{text: "prefix RetriableError: http2 stream closed", want: true},
		{text: "prefix RetriableError: stream closed", want: true},
		{text: "prefix RetriableError: CANCEL", want: true},
		{text: "I mentioned RetriableError in passing", want: false},
		{text: "http/2 stream closed", want: false},
		{text: "CANCEL", want: false},
		{text: "I'll run the tests", want: false},
		{text: "", want: false},
		{text: "   ", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.text, func(t *testing.T) {
			if got := IsTransportError(tt.text); got != tt.want {
				t.Fatalf("IsTransportError(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}
