// Package main tests Kiro's RefreshToken CBOR RPC using a CSRF token
// extracted from the authenticated HTML page.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
)

func main() {
	access := flag.String("access", "", "AccessToken cookie value")
	refresh := flag.String("refresh", "", "RefreshToken cookie value (required)")
	userID := flag.String("userid", "", "UserId cookie value")
	idp := flag.String("idp", "Google", "Idp cookie value")
	flag.Parse()

	if *refresh == "" {
		fmt.Fprintln(os.Stderr, "--refresh is required")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse("https://app.kiro.dev/")
	seed := []*http.Cookie{
		{Name: "RefreshToken", Value: *refresh, Domain: "app.kiro.dev", Path: "/", Secure: true, HttpOnly: true},
	}
	if *access != "" {
		seed = append(seed, &http.Cookie{Name: "AccessToken", Value: *access, Domain: "app.kiro.dev", Path: "/", Secure: true, HttpOnly: true})
	}
	if *userID != "" {
		seed = append(seed, &http.Cookie{Name: "UserId", Value: *userID, Domain: "app.kiro.dev", Path: "/", Secure: true, HttpOnly: true})
	}
	if *idp != "" {
		seed = append(seed, &http.Cookie{Name: "Idp", Value: *idp, Domain: "app.kiro.dev", Path: "/", Secure: true, HttpOnly: true})
	}
	jar.SetCookies(u, seed)

	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}

	// Step 1: fetch HTML to grab CSRF token.
	fmt.Println("[1] GET https://app.kiro.dev/  -> extract csrf-token meta")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://app.kiro.dev/", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html")
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetch html:", err)
		os.Exit(1)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 128*1024))
	resp.Body.Close()

	csrfRe := regexp.MustCompile(`<meta[^>]*name="csrf-token"[^>]*content="([^"]+)"`)
	m := csrfRe.FindStringSubmatch(string(body))
	if len(m) < 2 {
		fmt.Fprintln(os.Stderr, "csrf-token not found in HTML; are cookies valid?")
		os.Exit(1)
	}
	csrf := m[1]
	fmt.Printf("    csrf-token = %s\n\n", csrf)

	// Step 2: call RefreshToken CBOR RPC.
	fmt.Println("[2] POST /service/KiroWebPortalService/operation/RefreshToken")
	payload := map[string]any{"refreshToken": *refresh}
	cborBody, err := kiro.EncodeCBOR(payload)
	if err != nil {
		fmt.Fprintln(os.Stderr, "encode:", err)
		os.Exit(1)
	}

	rpcReq, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://app.kiro.dev/service/KiroWebPortalService/operation/RefreshToken",
		bytes.NewReader(cborBody))
	rpcReq.Header.Set("Content-Type", "application/cbor")
	rpcReq.Header.Set("Accept", "application/cbor")
	rpcReq.Header.Set("smithy-protocol", "rpc-v2-cbor")
	rpcReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36")
	rpcReq.Header.Set("x-amz-user-agent", "aws-sdk-js/1.0.0")
	rpcReq.Header.Set("Origin", "https://app.kiro.dev")
	rpcReq.Header.Set("Referer", "https://app.kiro.dev/")
	rpcReq.Header.Set("x-csrf-token", csrf)
	rpcReq.Header.Set("x-kiro-visitorid", fmt.Sprintf("%d-refreshprobe", time.Now().UnixMilli()))

	rpcResp, err := client.Do(rpcReq)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rpc:", err)
		os.Exit(1)
	}
	defer rpcResp.Body.Close()
	rpcBody, _ := io.ReadAll(io.LimitReader(rpcResp.Body, 128*1024))

	fmt.Printf("    status = %d\n", rpcResp.StatusCode)
	fmt.Printf("    content-type = %s\n", rpcResp.Header.Get("Content-Type"))
	for _, name := range []string{"x-amzn-requestid", "x-amzn-errortype", "x-amzn-errormessage"} {
		if v := rpcResp.Header.Get(name); v != "" {
			fmt.Printf("    %s = %s\n", name, v)
		}
	}
	fmt.Printf("    body bytes = %d\n\n", len(rpcBody))

	if rpcResp.StatusCode >= 400 {
		fmt.Println("    raw body preview:")
		if len(rpcBody) < 2048 {
			fmt.Println("    " + string(rpcBody))
		} else {
			fmt.Println("    " + string(rpcBody[:2048]) + "...")
		}
		os.Exit(2)
	}

	decoded, err := kiro.DecodeCBOR(rpcBody)
	if err != nil {
		fmt.Fprintln(os.Stderr, "decode cbor:", err)
		os.Exit(2)
	}
	// Capture Set-Cookie that came back.
	for _, ck := range rpcResp.Cookies() {
		fmt.Printf("    Set-Cookie: %s = %.40q ...\n", ck.Name, ck.Value)
	}
	pretty, _ := json.MarshalIndent(decoded, "    ", "  ")
	fmt.Println()
	fmt.Println("    decoded CBOR:")
	fmt.Println("    " + string(pretty))
}
