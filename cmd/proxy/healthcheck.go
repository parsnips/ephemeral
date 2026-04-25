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
	"strings"
	"time"
)

// runHealthcheck issues a GraphQL schema introspection query against the
// local proxy listener and exits 0 iff the response is a well-formed GraphQL
// envelope with no errors. That walks the full path: HTTP listener -> auth
// round tripper -> STS token -> upstream tenant.
//
// Designed to be wired up as a Docker HEALTHCHECK without needing curl/wget
// in the image. Writes failures to stderr and exits non-zero.
func runHealthcheck(args []string) {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	addr := fs.String("addr", envOr("HEALTHCHECK_ADDR", "http://localhost:8080"), "proxy HTTP listener to probe")
	path := fs.String("path", envOr("HEALTHCHECK_PATH", "/financial/v1/graphql"), "GraphQL path on the proxy")
	timeout := fs.Duration("timeout", 5*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		exitf("healthcheck: %v", err)
	}

	url := strings.TrimRight(*addr, "/") + *path
	body, _ := json.Marshal(map[string]string{
		"query": "{ __schema { queryType { name } } }",
	})

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		exitf("healthcheck: build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		exitf("healthcheck: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		exitf("healthcheck: read body: %v", err)
	}
	if resp.StatusCode/100 != 2 {
		exitf("healthcheck: status %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var env struct {
		Data struct {
			Schema struct {
				QueryType struct {
					Name string `json:"name"`
				} `json:"queryType"`
			} `json:"__schema"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		exitf("healthcheck: decode: %v (body=%s)", err, truncate(string(raw), 200))
	}
	if len(env.Errors) > 0 {
		msgs := make([]string, 0, len(env.Errors))
		for _, e := range env.Errors {
			msgs = append(msgs, e.Message)
		}
		exitf("healthcheck: graphql errors: %s", strings.Join(msgs, "; "))
	}
	if env.Data.Schema.QueryType.Name == "" {
		exitf("healthcheck: empty schema response: %s", truncate(string(raw), 200))
	}
}

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
