// Command kirotest sends a single Anthropic-style message through the Kiro
// pipeline end-to-end: transform -> generateAssistantResponse -> EventStream
// -> stdout Anthropic SSE. Not part of the production build; used to validate
// the upstream protocol shape with a real token.
//
// Usage:
//
//	go run ./cmd/kirotest -token "aoaAAAAA..." -profile "arn:aws:..." -q "hello"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
)

func main() {
	var (
		token   = flag.String("token", "", "Kiro accessToken (aoa...)")
		profile = flag.String("profile", "", "profileArn")
		model   = flag.String("model", "claude-sonnet-4.5", "kiro model id")
		prompt  = flag.String("q", "Say hi in 5 words.", "prompt")
	)
	flag.Parse()
	if *token == "" || *profile == "" {
		fmt.Fprintln(os.Stderr, "-token and -profile are required")
		os.Exit(2)
	}

	req := &kiro.AnthropicRequest{
		Model:     *model,
		MaxTokens: 128,
		Messages: []kiro.AnthropicMessage{{
			Role:    "user",
			Content: []byte(`"` + escapeJSON(*prompt) + `"`),
		}},
	}

	payload, err := kiro.BuildKiroPayload(req, kiro.BuildOptions{
		ProfileArn: *profile,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "build payload:", err)
		os.Exit(1)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal json:", err)
		os.Exit(1)
	}

	// Directly hit the Q API streaming endpoint.
	url := "https://q.us-east-1.amazonaws.com/generateAssistantResponse"
	httpReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "build http req:", err)
		os.Exit(1)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/vnd.amazon.eventstream")
	httpReq.Header.Set("Authorization", "Bearer "+*token)
	httpReq.Header.Set("x-amz-user-agent", "aws-sdk-js/1.0.27")
	httpReq.Header.Set("User-Agent", "aws-sdk-js/1.0.27 KiroIDE-0.1.29-test")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		fmt.Fprintln(os.Stderr, "http do:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	fmt.Fprintf(os.Stderr, "HTTP %d %s\n", resp.StatusCode, resp.Header.Get("Content-Type"))
	if resp.StatusCode != 200 {
		buf := make([]byte, 4096)
		n, _ := resp.Body.Read(buf)
		fmt.Fprintf(os.Stderr, "BODY: %s\n", string(buf[:n]))
		os.Exit(1)
	}

	reader := kiro.NewEventStreamReader(resp.Body)
	frameIdx := 0
	for {
		msg, err := reader.Next()
		if err != nil {
			fmt.Fprintln(os.Stderr, "next frame:", err)
			break
		}
		frameIdx++
		fmt.Fprintf(os.Stderr, "[frame %d] event=%s msg=%s headers=%d payload=%dB\n",
			frameIdx, msg.EventType(), msg.MessageType(), len(msg.Headers), len(msg.Payload))
		if len(msg.Payload) > 0 && len(msg.Payload) < 1024 {
			fmt.Fprintf(os.Stderr, "           payload: %s\n", string(msg.Payload))
		}
	}
}

func escapeJSON(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
