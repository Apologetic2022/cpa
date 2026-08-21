// Command cacheprobe measures Cursor provider prompt-cache behaviour across
// gateway turns. It exchanges a Cursor API key for an access token, then runs
// a three-turn conversation the way the gateway does (each turn is a separate
// Agent run carrying the full history) and prints the upstream usage,
// including cache_read tokens, for every turn.
//
//	CURSOR_API_KEY=crsr_...  [PROBE_MODEL=grok-4.6]  [CPA_CURSOR_CONV_REUSE=0]  cacheprobe
//
// With conversation reuse enabled (default) follow-up turns should report
// cache_read > 0; with CPA_CURSOR_CONV_REUSE=0 they replicate the fresh-
// conversation-per-turn behaviour and show whether the provider cache is lost.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	cursorauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
	log "github.com/sirupsen/logrus"
)

func main() {
	if strings.TrimSpace(os.Getenv("PROBE_QUIET")) == "" {
		log.SetLevel(log.DebugLevel)
	}
	apiKey := strings.TrimSpace(os.Getenv("CURSOR_API_KEY"))
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "CURSOR_API_KEY is required")
		os.Exit(2)
	}
	model := strings.TrimSpace(os.Getenv("PROBE_MODEL"))
	if model == "" {
		model = "grok-4.6"
	}
	ctx := context.Background()

	svc := cursorauth.NewAuthService()
	tok, err := svc.RefreshToken(ctx, apiKey, "", "")
	must("token exchange", err)
	fmt.Printf("token ok (expires %s)\n", tok.ExpiresAt.Format(time.RFC3339))

	creds := cursorlib.CredentialsFromMetadata(map[string]any{
		"access_token": tok.AccessToken,
		"api_key":      apiKey,
	})

	system := strings.Repeat(
		"You are an expert software engineering assistant for the ACME monorepo. "+
			"Follow the house style guide: tabs for indentation, table-driven tests, "+
			"contexts threaded through every call, errors wrapped with fmt.Errorf and %w, "+
			"and public identifiers documented with full sentences. ", 40)

	msgs := []cursorlib.ChatMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: "Reply with exactly the word: ONE"},
	}
	r1, err := cursorlib.RunChat(ctx, creds, model, msgs, nil)
	must("turn1", err)
	report("turn1", r1)

	time.Sleep(3 * time.Second)

	msgs2 := append(append([]cursorlib.ChatMessage(nil), msgs...),
		cursorlib.ChatMessage{Role: "assistant", Content: r1.Text},
		cursorlib.ChatMessage{Role: "user", Content: "Reply with exactly the word: TWO"},
	)
	r2, err := cursorlib.RunChat(ctx, creds, model, msgs2, nil)
	must("turn2", err)
	report("turn2", r2)

	time.Sleep(3 * time.Second)

	msgs3 := append(append([]cursorlib.ChatMessage(nil), msgs2...),
		cursorlib.ChatMessage{Role: "assistant", Content: r2.Text},
		cursorlib.ChatMessage{Role: "user", Content: "Reply with exactly the word: THREE"},
	)
	r3, err := cursorlib.RunChat(ctx, creds, model, msgs3, nil)
	must("turn3", err)
	report("turn3", r3)
}

func must(label string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s failed: %v\n", label, err)
		os.Exit(1)
	}
}

func report(label string, r *cursorlib.ChatResult) {
	text := strings.TrimSpace(r.Text)
	if len(text) > 60 {
		text = text[:60] + "…"
	}
	fmt.Printf("%s: conv=%s in=%d out=%d cache_read=%d cache_write=%d reasoning=%d finish=%s text=%q\n",
		label, r.ConversationID, r.InputTokens, r.OutputTokens,
		r.CacheReadTokens, r.CacheWriteTokens, r.ReasoningTokens, r.FinishReason, text)
}
