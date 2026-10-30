package queue

import (
	"errors"
	"fmt"
	"testing"
)

func TestOutcomeFor(t *testing.T) {
	boom := errors.New("timeout")
	gone := Permanent(errors.New("account not found"))

	tests := []struct {
		name      string
		err       error
		delivered uint64
		max       uint64
		want      Outcome
	}{
		{"success", nil, 1, 5, OutcomeAck},
		{"transient first try", boom, 1, 5, OutcomeRetry},
		{"transient before the cap", boom, 4, 5, OutcomeRetry},
		{"transient at the cap", boom, 5, 5, OutcomeDead},
		{"permanent on the first try", gone, 1, 5, OutcomeDead},
		{"wrapped permanent", fmt.Errorf("sync: %w", gone), 1, 5, OutcomeDead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := OutcomeFor(tt.err, tt.delivered, tt.max); got != tt.want {
				t.Fatalf("outcome = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPermanentNil(t *testing.T) {
	if err := Permanent(nil); err != nil {
		t.Fatalf("Permanent(nil) = %v", err)
	}
	err := Permanent(errors.New("bad json"))
	if Permanent(err) != err {
		t.Fatal("wrapping twice should keep the same error")
	}
	if !IsPermanent(fmt.Errorf("job: %w", err)) {
		t.Fatal("permanent marker was lost")
	}
}
