package main

import (
	"testing"
	"time"
)

func TestLivenessStaleAfter(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{"short interval clamps to floor", 10 * time.Second, 2 * time.Minute},
		{"floor boundary", 40 * time.Second, 2 * time.Minute},
		{"default interval scales", 60 * time.Second, 3 * time.Minute},
		{"long interval scales", 5 * time.Minute, 15 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := livenessStaleAfter(tt.interval); got != tt.want {
				t.Errorf("livenessStaleAfter(%s) = %s, want %s", tt.interval, got, tt.want)
			}
		})
	}
}
