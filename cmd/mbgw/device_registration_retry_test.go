package main

import (
	"testing"
	"time"
)

func TestNextDeviceRegistrationRetryDelay(t *testing.T) {
	tests := []struct {
		current time.Duration
		want    time.Duration
	}{
		{0, 5 * time.Second},
		{5 * time.Second, 10 * time.Second},
		{10 * time.Second, 20 * time.Second},
		{40 * time.Second, 80 * time.Second},
		{160 * time.Second, 5 * time.Minute},
		{5 * time.Minute, 5 * time.Minute},
	}

	for _, tt := range tests {
		if got := nextDeviceRegistrationRetryDelay(tt.current); got != tt.want {
			t.Fatalf("nextDeviceRegistrationRetryDelay(%s) = %s, want %s", tt.current, got, tt.want)
		}
	}
}
