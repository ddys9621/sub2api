// Package main provides a one-off CLI that probes the Kiro CodeWhisperer
// /generateAssistantResponse endpoint with a real accessToken + profileArn.
//
// Usage:
//
//	go run ./cmd/kiroprobe --token "aoaAAAA..." --profile "arn:aws:..."
//	    [--model claude-sonnet-4.5] [--prompt "Hello"]
//
// The output is the raw AWS EventStream frame decoded via the sub2api kiro
// pkg, plus any assistant text extracted from assistantResponseEvent frames.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/google/uuid"
)

func main() {
	token := flag.String("token", "", "Kiro accessToken (from ExchangeToken response or AccessToken cookie)")
	profile := flag.String("profile", "", "profileArn, e.g. arn:aws:codewhisperer:us-east-1:699475941385:profile/EHGA3GRVQMUK")
	model := flag.String("model", "claude-sonnet-4.5", "Kiro model id")
	prompt := flag.String("prompt", "Hi, say hello in one short sentence.", "prompt text")
	region := flag.String("region", "us-east-1", "AWS region")
	flag.Parse()

	if *token == "" || *profile == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --token and --profile are required")
		flag.Usage()
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	payload := map[string]any{
		"profileArn": *profile,
		"conversationState": map[string]any{
			"chatTriggerType": "MANUAL",
			"conversationId":  uuid.NewString(),
			"currentMessage": map[string]any{
				"userInputMessage": map[string]any{
					"content": *prompt,
					"modelId": *model,
					"origin":  "AI_EDITOR",
				},
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		die("marshal payload: %v", err)
	}
	fmt.Printf("[req] POST https://codewhisperer.%s.amazonaws.com/generateAssistantResponse\n", *region)
	fmt.Printf("[req] model=%s profileArn=%s\n", *model, *profile)
	fmt.Printf("[req] payload (%d bytes): %s\n\n", len(body), string(body))

	urls := []string{
		fmt.Sprintf("https://codewhisperer.%s.amazonaws.com/generateAssistantResponse", *region),
		fmt.Sprintf("https://q.%s.amazonaws.com/generateAssistantResponse", *region),
	}

	client := &http.Client{Timeout: 60 * time.Second}

	for _, u := range urls {
		fmt.Printf("===== try %s =====\n", u)
		if err := probe(ctx, client, u, *token, body); err != nil {
			fmt.Printf("[err] %v\n\n", err)
			continue
		}
		return
	}
}

func probe(ctx context.Context, client *http.Client, url, token string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "aws-sdk-js/1.0.27 KiroIDE-0.7.45-sub2api-probe")
	req.Header.Set("x-amz-user-agent", "aws-sdk-js/1.0.27 KiroIDE-0.7.45-sub2api-probe")
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	req.Header.Set("amz-sdk-invocation-id", uuid.NewString())
	req.Header.Set("amz-sdk-request", "attempt=1; max=3")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	fmt.Printf("[resp] status=%d content-type=%q\n", resp.StatusCode, resp.Header.Get("Content-Type"))
	for k, v := range resp.Header {
		fmt.Printf("[resp] %s: %s\n", k, v)
	}
	fmt.Println()

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(raw))
	}

	r := kiro.NewEventStreamReader(resp.Body)
	var text bytes.Buffer
	frameCount := 0
	for {
		msg, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("eventstream: %w (text so far: %q)", err, text.String())
		}
		frameCount++
		et := msg.EventType()
		mt := msg.MessageType()
		fmt.Printf("[frame %d] :event-type=%s :message-type=%s payload=%d bytes\n", frameCount, et, mt, len(msg.Payload))
		if len(msg.Payload) < 400 {
			fmt.Printf("            payload: %s\n", string(msg.Payload))
		} else {
			fmt.Printf("            payload-head: %s...\n", string(msg.Payload[:400]))
		}
		if et == "assistantResponseEvent" && len(msg.Payload) > 0 {
			var p struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(msg.Payload, &p); err == nil {
				text.WriteString(p.Content)
			}
		}
	}
	fmt.Println()
	fmt.Println("===== assistant text =====")
	fmt.Println(text.String())
	return nil
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
	os.Exit(1)
}
