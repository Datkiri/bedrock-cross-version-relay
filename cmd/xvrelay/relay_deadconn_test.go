package main

import (
	"context"
	"errors"
	"net"
	"testing"
)

// TestConnDeadRecognisesClosedDestination pins the fix for the WARN storm seen
// when a NetherNet client disconnected: go-nethernet reports
// "nethernet: SCTP transport closed" as a PLAIN error (errors.New, no
// sentinel), so errors.Is alone cannot tell that the destination is gone.
// minecraft.Conn.Context() is canceled on close, so the write loop can stop.
func TestConnDeadRecognisesClosedDestination(t *testing.T) {
	plain := errors.New("write packet minecraft 1793884236535645300: nethernet: SCTP transport closed")

	if connDead(plain, context.Background()) {
		t.Fatal("a plain transport error on a live destination must not be terminal")
	}
	if connDead(plain, nil) {
		t.Fatal("a plain transport error without a destination context must not be terminal")
	}

	// The classic sentinels keep working.
	for _, err := range []error{
		net.ErrClosed,
		context.Canceled,
		context.DeadlineExceeded,
	} {
		if !connDead(err, context.Background()) {
			t.Fatalf("%v must still be terminal", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !connDead(plain, ctx) {
		t.Fatal("a canceled destination context must make any write error terminal")
	}
}
