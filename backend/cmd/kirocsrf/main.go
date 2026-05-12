// Package main probes the authenticated Kiro Web Portal HTML to locate the
// CSRF token meta tag. Kiro's RefreshToken CBOR RPC requires an x-csrf-token
// header whose value is injected into the root HTML by the server once the
// AccessToken / RefreshToken / UserId / Idp cookies are set.
//
// Usage:
//
//	go run ./cmd/kirocsrf --access "..." --refresh "..." --userid "..." [--idp Google]
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

func main() {
	access := flag.String("access", "", "AccessToken cookie value")
	refresh := flag.String("refresh", "", "RefreshToken cookie value")
	userID := flag.String("userid", "", "UserId cookie value")
	idp := flag.String("idp", "Google", "Idp cookie value")
	target := flag.String("url", "https://app.kiro.dev/", "URL to fetch")
	flag.Parse()

	if *access == "" {
		fmt.Fprintln(os.Stderr, "--access is required")
		flag.Usage()
		os.Exit(1)
	}

	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse("https://app.kiro.dev/")
	setCookies := []*http.Cookie{
		{Name: "AccessToken", Value: *access, Domain: "app.kiro.dev", Path: "/", Secure: true, HttpOnly: true},
	}
	if *refresh != "" {
		setCookies = append(setCookies, &http.Cookie{Name: "RefreshToken", Value: *refresh, Domain: "app.kiro.dev", Path: "/", Secure: true, HttpOnly: true})
	}
	if *userID != "" {
		setCookies = append(setCookies, &http.Cookie{Name: "UserId", Value: *userID, Domain: "app.kiro.dev", Path: "/", Secure: true, HttpOnly: true})
	}
	if *idp != "" {
		setCookies = append(setCookies, &http.Cookie{Name: "Idp", Value: *idp, Domain: "app.kiro.dev", Path: "/", Secure: true, HttpOnly: true})
	}
	jar.SetCookies(u, setCookies)

	client := &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
	}

	req, err := http.NewRequest(http.MethodGet, *target, nil)
	if err != nil {
		panic(err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	fmt.Printf("[resp] status=%d ct=%q bytes=%d\n\n", resp.StatusCode, resp.Header.Get("Content-Type"), len(body))

	// All meta tags
	metaRe := regexp.MustCompile(`(?s)<meta[^>]*>`)
	allMetas := metaRe.FindAllString(string(body), -1)
	fmt.Printf("----- %d meta tags -----\n", len(allMetas))
	for _, m := range allMetas {
		fmt.Println(m)
	}
	fmt.Println()

	// Locate user-status
	userStatusRe := regexp.MustCompile(`<meta[^>]*name="user-status"[^>]*content="([^"]+)"`)
	if match := userStatusRe.FindStringSubmatch(string(body)); len(match) > 1 {
		fmt.Printf("user-status: %s\n", match[1])
	}

	// Hunt for CSRF patterns
	patterns := []string{
		`<meta[^>]*name="csrf-token"[^>]*content="([^"]+)"`,
		`<meta[^>]*content="([^"]+)"[^>]*name="csrf-token"`,
		`<meta[^>]*name="csrf"[^>]*content="([^"]+)"`,
		`<meta[^>]*name="x-csrf-token"[^>]*content="([^"]+)"`,
		`window\.__CSRF_TOKEN__\s*=\s*"([^"]+)"`,
		`csrfToken["']?\s*[:=]\s*["']([^"']+)["']`,
	}
	fmt.Println("----- CSRF hunting -----")
	for _, pat := range patterns {
		re := regexp.MustCompile(pat)
		if m := re.FindStringSubmatch(string(body)); len(m) > 1 {
			fmt.Printf("  [HIT] pattern %q -> %s\n", pat, m[1])
		}
	}

	// Dump the first 4KB of HTML for manual inspection.
	fmt.Println()
	fmt.Println("----- HTML head preview -----")
	head := string(body)
	if i := strings.Index(head, "</head>"); i > 0 && i < 8*1024 {
		fmt.Println(head[:i+len("</head>")])
	} else if len(head) > 4096 {
		fmt.Println(head[:4096] + "...[truncated]")
	} else {
		fmt.Println(head)
	}
}
