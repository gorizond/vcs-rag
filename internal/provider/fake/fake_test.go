package fake

import (
	"context"
	"errors"
	"testing"
)

// TestErrorInjection checks that the injected error reaches the caller.
func TestErrorInjection(t *testing.T) {
	want := errors.New("boom")
	p := &Provider{ErrListRepositories: want}
	if _, err := p.ListRepositories(context.Background()); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}
