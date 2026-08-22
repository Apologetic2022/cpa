package cursor

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newEventSession() *Session {
	return &Session{
		ID:     "s-teardown",
		events: make(chan StreamEvent, 8),
		errCh:  make(chan error, 1),
	}
}

// Closing the Agent stream after a turn ends makes the in-flight read fail.
// That error arrives after the terminal event and must not reach the caller,
// or a complete answer is reported as a broken stream.
func TestFailAfterFinishedTurnIsNotReported(t *testing.T) {
	session := newEventSession()
	session.markFinished()
	session.fail(errors.New("http2: response body closed"))

	select {
	case err := <-session.errCh:
		t.Fatalf("teardown error surfaced: %v", err)
	default:
	}
	select {
	case ev := <-session.events:
		t.Fatalf("teardown emitted %q event", ev.Type)
	default:
	}
}

func TestFailBeforeTerminalEventStillReports(t *testing.T) {
	session := newEventSession()
	session.fail(errors.New("upstream exploded"))

	select {
	case err := <-session.errCh:
		if err == nil {
			t.Fatal("expected the failure to be reported")
		}
	default:
		t.Fatal("mid-stream failure was swallowed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var kinds []string
	for len(kinds) < 2 {
		select {
		case ev := <-session.events:
			kinds = append(kinds, ev.Type)
		case <-ctx.Done():
			t.Fatalf("expected error and segment_end events, got %v", kinds)
		}
	}
	if kinds[0] != "error" || kinds[1] != "segment_end" {
		t.Fatalf("unexpected events: %v", kinds)
	}
}
