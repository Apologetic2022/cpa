package cursor

import (
	"bytes"
	"testing"
)

func TestChecksumDeterministicPacking(t *testing.T) {
	got := Checksum("abc", "def", 1_700_000_000_000)
	if got == "" {
		t.Fatal("empty checksum")
	}
	if got[len(got)-7:] != "abc/def" {
		t.Fatalf("suffix = %q, want abc/def", got[len(got)-7:])
	}
	again := Checksum("abc", "def", 1_700_000_000_000)
	if got != again {
		t.Fatalf("checksum not stable: %q vs %q", got, again)
	}
}

func TestEncodeDecodeConnectEnvelope(t *testing.T) {
	payload := []byte("hello-cursor")
	frame := EncodeEnvelope(payload, false)
	if frame[0]&flagCompressed != 0 {
		t.Fatal("default EncodeEnvelope must not compress outbound frames")
	}
	dec := NewDecoder()
	envs, err := dec.Feed(frame)
	if err != nil {
		t.Fatalf("Feed() error: %v", err)
	}
	if len(envs) != 1 {
		t.Fatalf("len(envs)=%d", len(envs))
	}
	if string(envs[0].Payload) != string(payload) {
		t.Fatalf("payload = %q", envs[0].Payload)
	}
}

func TestEncodeEnvelopeMaybeCompressRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte("cursor-compress-"), 128) // > 1KiB
	frame := EncodeEnvelopeMaybeCompress(payload, false, true)
	if frame[0]&flagCompressed == 0 {
		t.Fatal("expected compressed flag")
	}
	dec := NewDecoder()
	envs, err := dec.Feed(frame)
	if err != nil {
		t.Fatalf("Feed() error: %v", err)
	}
	if len(envs) != 1 || string(envs[0].Payload) != string(payload) {
		t.Fatalf("round-trip failed: len=%d", len(envs))
	}
}
