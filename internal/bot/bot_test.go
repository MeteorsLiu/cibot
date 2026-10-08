package bot

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v83/github"
)

func TestServeHTTP(t *testing.T) {
	secret := []byte("test-webhook-secret")
	for _, tc := range []struct {
		name        string
		method      string
		contentType string
		event       string
		body        string
		secret      []byte
		signature   string
		want        int
	}{
		{name: "method", method: http.MethodGet, want: http.StatusMethodNotAllowed},
		{name: "missing secret", method: http.MethodPost, want: http.StatusInternalServerError},
		{name: "missing signature", method: http.MethodPost, contentType: "application/json", event: "ping", body: `{}`, secret: secret, want: http.StatusForbidden},
		{name: "wrong signature", method: http.MethodPost, contentType: "application/json", event: "ping", body: `{}`, secret: secret, signature: "sha256=" + strings.Repeat("0", 64), want: http.StatusForbidden},
		{name: "malformed JSON", method: http.MethodPost, contentType: "application/json", event: "ping", body: `{`, secret: secret, signature: "valid", want: http.StatusBadRequest},
		{name: "unknown event", method: http.MethodPost, contentType: "application/json", event: "unknown-event", body: `{}`, secret: secret, signature: "valid", want: http.StatusBadRequest},
		{name: "unsupported content type", method: http.MethodPost, contentType: "text/plain", event: "ping", body: `{}`, secret: secret, signature: "valid", want: http.StatusForbidden},
		{name: "ping", method: http.MethodPost, contentType: "application/json", event: "ping", body: `{"zen":"hello","installation":{"id":7}}`, secret: secret, signature: "valid", want: http.StatusOK},
		{name: "raw whitespace", method: http.MethodPost, contentType: "application/json", event: "ping", body: " {\n  \"zen\": \"hello\"\n } ", secret: secret, signature: "valid", want: http.StatusOK},
		{name: "form payload", method: http.MethodPost, contentType: "application/x-www-form-urlencoded", event: "ping", body: url.Values{"payload": {`{"zen":"hello"}`}}.Encode(), secret: secret, signature: "valid", want: http.StatusOK},
		{name: "ignored event", method: http.MethodPost, contentType: "application/json", event: "issues", body: `{"action":"opened"}`, secret: secret, signature: "valid", want: http.StatusNoContent},
		{name: "empty PR event", method: http.MethodPost, contentType: "application/json", event: "pull_request", body: `{}`, secret: secret, signature: "valid", want: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, "/", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", tc.contentType)
			request.Header.Set(github.EventTypeHeader, tc.event)
			if tc.signature == "valid" {
				request.Header.Set(github.SHA256SignatureHeader, signPayload(tc.secret, []byte(tc.body)))
			} else if tc.signature != "" {
				request.Header.Set(github.SHA256SignatureHeader, tc.signature)
			}
			response := httptest.NewRecorder()
			(&Bot{WebhookSecret: string(tc.secret)}).ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, tc.want, response.Body.String())
			}
			if tc.method != http.MethodPost && response.Header().Get("Allow") != http.MethodPost {
				t.Fatal("missing Allow: POST")
			}
		})
	}

	t.Run("tampered body", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"zen":"changed"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(github.EventTypeHeader, "ping")
		request.Header.Set(github.SHA256SignatureHeader, signPayload(secret, []byte(`{"zen":"original"}`)))
		response := httptest.NewRecorder()
		(&Bot{WebhookSecret: string(secret)}).ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want forbidden", response.Code)
		}
	})

	t.Run("concurrent deliveries", func(t *testing.T) {
		bot := &Bot{WebhookSecret: string(secret)}
		body := []byte(`{"zen":"hello"}`)
		signature := signPayload(secret, body)
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set(github.EventTypeHeader, "ping")
				request.Header.Set(github.SHA256SignatureHeader, signature)
				response := httptest.NewRecorder()
				bot.ServeHTTP(response, request)
				if response.Code != http.StatusOK {
					t.Errorf("status = %d, want OK", response.Code)
				}
			}()
		}
		wg.Wait()
	})

	t.Run("constructor", func(t *testing.T) {
		bot, _ := newTestBot(t, string(secret), func(w http.ResponseWriter, r *http.Request) {
			t.Error("constructor or ping made an API request")
			w.WriteHeader(http.StatusInternalServerError)
		})
		if bot.WebhookSecret != string(secret) || bot.transport == nil || bot.clients == nil || len(bot.clients) != 0 {
			t.Fatal("constructor did not initialize the bot")
		}
		request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(github.EventTypeHeader, "ping")
		request.Header.Set(github.SHA256SignatureHeader, signPayload(secret, []byte(`{}`)))
		response := httptest.NewRecorder()
		bot.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, want OK", response.Code)
		}
		if len(bot.clients) != 0 {
			t.Fatal("ping initialized an installation client")
		}

		missing := filepath.Join(t.TempDir(), "missing.pem")
		if bot, err := New(string(secret), 123, missing); err == nil || bot != nil {
			t.Fatal("missing key must return a nil bot and an error")
		}
		invalid := filepath.Join(t.TempDir(), "invalid.pem")
		if err := os.WriteFile(invalid, []byte("not a private key"), 0600); err != nil {
			t.Fatal(err)
		}
		if bot, err := New(string(secret), 123, invalid); err == nil || bot != nil {
			t.Fatal("invalid key must return a nil bot and an error")
		}
	})
}

func TestWebhookRouting(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	secret := []byte("test-webhook-secret")
	var apiRequests atomic.Int64
	bot, _ := newTestBot(t, string(secret), func(w http.ResponseWriter, r *http.Request) {
		apiRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && (r.URL.Path == "/app/installations/7/access_tokens" || r.URL.Path == "/app/installations/8/access_tokens"):
			id := strings.Split(r.URL.Path, "/")[3]
			fmt.Fprintf(w, `{"token":"installation-%s","expires_at":%q}`, id, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case r.Method == http.MethodGet && (r.URL.Path == "/repos/llarhub/.index/pulls/3/files" || r.URL.Path == "/repos/MeteorsLiu/llarhub/pulls/3/files"):
			wantToken := "token installation-7"
			if strings.HasPrefix(r.URL.Path, "/repos/MeteorsLiu/") {
				wantToken = "token installation-8"
			}
			if r.Header.Get("Authorization") != wantToken {
				t.Error("PR files request did not use the installation token")
			}
			io.WriteString(w, `[]`)
		default:
			t.Errorf("unexpected API request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	for _, tc := range []struct {
		name         string
		event        string
		body         string
		want         int
		wantRequests int64
	}{
		{name: "production merged PR", event: "pull_request", body: `{"action":"closed","number":3,"pull_request":{"merged":true,"base":{"ref":"main"}},"repository":{"full_name":"llarhub/.index"},"installation":{"id":7}}`, want: http.StatusOK, wantRequests: 2},
		{name: "test merged PR", event: "pull_request", body: `{"action":"closed","number":3,"pull_request":{"merged":true,"base":{"ref":"main"}},"repository":{"full_name":"MeteorsLiu/llarhub"},"installation":{"id":8}}`, want: http.StatusOK, wantRequests: 2},
		{name: "repeat production merged PR", event: "pull_request", body: `{"action":"closed","number":3,"pull_request":{"merged":true,"base":{"ref":"main"}},"repository":{"full_name":"llarhub/.index"},"installation":{"id":7}}`, want: http.StatusOK, wantRequests: 1},
		{name: "closed without merging", event: "pull_request", body: `{"action":"closed","pull_request":{"merged":false,"base":{"ref":"main"}},"repository":{"full_name":"llarhub/.index"}}`, want: http.StatusNoContent},
		{name: "opened PR", event: "pull_request", body: `{"action":"opened","pull_request":{"merged":false,"base":{"ref":"main"}},"repository":{"full_name":"llarhub/.index"}}`, want: http.StatusNoContent},
		{name: "different base", event: "pull_request", body: `{"action":"closed","pull_request":{"merged":true,"base":{"ref":"dev"}},"repository":{"full_name":"llarhub/.index"}}`, want: http.StatusNoContent},
		{name: "different registry", event: "pull_request", body: `{"action":"closed","pull_request":{"merged":true,"base":{"ref":"main"}},"repository":{"full_name":"xgo-dev/llarhub"}}`, want: http.StatusNoContent},
		{name: "main push", event: "push", body: `{"ref":"refs/heads/main","repository":{"full_name":"llarhub/libfoo"}}`, want: http.StatusNoContent},
		{name: "deleted c branch", event: "push", body: `{"ref":"refs/heads/c","deleted":true,"repository":{"full_name":"llarhub/libfoo"}}`, want: http.StatusNoContent},
		{name: "other owner main push", event: "push", body: `{"ref":"refs/heads/main","repository":{"full_name":"other/libfoo"}}`, want: http.StatusNoContent},
		{name: "other owner deleted c branch", event: "push", body: `{"ref":"refs/heads/c","deleted":true,"repository":{"full_name":"other/libfoo"}}`, want: http.StatusNoContent},
		{name: "empty push", event: "push", body: `{}`, want: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := apiRequests.Load()
			beforeClients := len(bot.clients)
			beforeClient := bot.clients[7]
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(github.EventTypeHeader, tc.event)
			request.Header.Set(github.SHA256SignatureHeader, signPayload(secret, []byte(tc.body)))
			response := httptest.NewRecorder()
			bot.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, tc.want, response.Body.String())
			}
			if got := apiRequests.Load() - before; got != tc.wantRequests {
				t.Fatalf("API requests = %d, want %d", got, tc.wantRequests)
			}
			if tc.wantRequests == 0 && len(bot.clients) != beforeClients {
				t.Fatal("irrelevant event initialized an installation client")
			}
			if tc.name == "repeat production merged PR" && (beforeClient == nil || bot.clients[7] != beforeClient) {
				t.Fatal("repeated delivery did not reuse its installation client")
			}
		})
	}

	t.Run("merged project detection", func(t *testing.T) {
		const mergeSHA = "0123456789abcdef0123456789abcdef01234567"
		for _, tc := range []struct {
			name             string
			owner            string
			index            string
			pages            []string
			tree             string
			tokenStatus      int
			filesStatus      int
			secondPageStatus int
			treeStatus       int
			repoStatus       int
			expireToken      bool
			cancel           bool
			want             int
			wantFiles        int
			wantTree         int
			wantRepos        []string
		}{
			{
				name: "pagination and deduplication", owner: "llarhub", index: ".index",
				pages: []string{
					`[{"filename":"libfoo/llcppg.cfg","status":"added"},{"filename":"README.md","status":"modified"},{"filename":".github/workflows/test.yml","status":"modified"}]`,
					`[{"filename":"libfoo/v1/formula.gox","status":"modified"},{"filename":"libbar/versions.json","status":"added"}]`,
				},
				tree: `{"tree":[{"path":"libfoo","type":"tree"},{"path":"libbar","type":"tree"}],"truncated":false}`,
				want: http.StatusOK, wantFiles: 2, wantTree: 1, wantRepos: []string{"llarhub/libfoo", "llarhub/libbar"},
			},
			{
				name: "test owner and repository 404", owner: "MeteorsLiu", index: "llarhub",
				pages: []string{`[{"filename":"libfoo/llcppg.cfg","status":"added"}]`},
				tree:  `{"tree":[{"path":"libfoo","type":"tree"}]}`, repoStatus: http.StatusNotFound,
				want: http.StatusOK, wantFiles: 1, wantTree: 1, wantRepos: []string{"MeteorsLiu/libfoo"},
			},
			{
				name: "rename checks both existing projects", owner: "MeteorsLiu", index: "llarhub",
				pages: []string{`[{"filename":"new/llcppg.cfg","previous_filename":"old/llcppg.cfg","status":"renamed"}]`},
				tree:  `{"tree":[{"path":"new","type":"tree"},{"path":"old","type":"tree"}]}`,
				want:  http.StatusOK, wantFiles: 1, wantTree: 1, wantRepos: []string{"MeteorsLiu/new", "MeteorsLiu/old"},
			},
			{
				name: "rename excludes removed project", owner: "MeteorsLiu", index: "llarhub",
				pages: []string{`[{"filename":"new/llcppg.cfg","previous_filename":"old/llcppg.cfg","status":"renamed"}]`},
				tree:  `{"tree":[{"path":"new","type":"tree"}]}`,
				want:  http.StatusOK, wantFiles: 1, wantTree: 1, wantRepos: []string{"MeteorsLiu/new"},
			},
			{
				name: "removed directory", owner: "MeteorsLiu", index: "llarhub",
				pages: []string{`[{"filename":"old/llcppg.cfg","status":"removed"}]`},
				tree:  `{"tree":[{"path":"unchanged","type":"tree"}]}`,
				want:  http.StatusOK, wantFiles: 1, wantTree: 1,
			},
			{
				name: "removed file in remaining project", owner: "MeteorsLiu", index: "llarhub",
				pages: []string{`[{"filename":"libfoo/old.cfg","status":"removed"}]`},
				tree:  `{"tree":[{"path":"libfoo","type":"tree"}]}`,
				want:  http.StatusOK, wantFiles: 1, wantTree: 1, wantRepos: []string{"MeteorsLiu/libfoo"},
			},
			{
				name: "toc and hidden directories only", owner: "MeteorsLiu", index: "llarhub",
				pages: []string{`[{"filename":"llarhub.toc","status":"modified"},{"filename":".claude/skills/tool.md","status":"added"},{"filename":".agents/skills/tool.md","status":"added"}]`},
				want:  http.StatusOK, wantFiles: 1,
			},
			{
				name: "non-directory root entries", owner: "MeteorsLiu", index: "llarhub",
				pages: []string{`[{"filename":"submodule/config","status":"removed"},{"filename":"symlink/config","status":"removed"}]`},
				tree:  `{"tree":[{"path":"submodule","type":"commit","mode":"160000"},{"path":"symlink","type":"blob","mode":"120000"}]}`,
				want:  http.StatusOK, wantFiles: 1, wantTree: 1,
			},
			{
				name: "installation authentication failure", owner: "MeteorsLiu", index: "llarhub",
				tokenStatus: http.StatusUnauthorized, want: http.StatusInternalServerError,
			},
			{
				name: "PR files forbidden", owner: "MeteorsLiu", index: "llarhub",
				filesStatus: http.StatusForbidden, want: http.StatusInternalServerError, wantFiles: 1,
			},
			{
				name: "PR files not found", owner: "MeteorsLiu", index: "llarhub",
				filesStatus: http.StatusNotFound, want: http.StatusInternalServerError, wantFiles: 1,
			},
			{
				name: "second page failure", owner: "MeteorsLiu", index: "llarhub",
				pages: []string{`[{"filename":"libfoo/config"}]`, `[]`}, secondPageStatus: http.StatusServiceUnavailable,
				want: http.StatusInternalServerError, wantFiles: 2,
			},
			{
				name: "merge tree failure", owner: "MeteorsLiu", index: "llarhub",
				pages: []string{`[{"filename":"libfoo/config"}]`}, treeStatus: http.StatusServiceUnavailable,
				want: http.StatusInternalServerError, wantFiles: 1, wantTree: 1,
			},
			{
				name: "truncated merge tree", owner: "MeteorsLiu", index: "llarhub",
				pages: []string{`[{"filename":"libfoo/config"}]`}, tree: `{"tree":[{"path":"libfoo","type":"tree"}],"truncated":true}`,
				want: http.StatusInternalServerError, wantFiles: 1, wantTree: 1,
			},
			{
				name: "repository forbidden", owner: "MeteorsLiu", index: "llarhub",
				pages: []string{`[{"filename":"libfoo/config"}]`}, tree: `{"tree":[{"path":"libfoo","type":"tree"}]}`, repoStatus: http.StatusForbidden,
				want: http.StatusInternalServerError, wantFiles: 1, wantTree: 1, wantRepos: []string{"MeteorsLiu/libfoo"},
			},
			{
				name: "repository unavailable", owner: "MeteorsLiu", index: "llarhub",
				pages: []string{`[{"filename":"libfoo/config"}]`}, tree: `{"tree":[{"path":"libfoo","type":"tree"}]}`, repoStatus: http.StatusServiceUnavailable,
				want: http.StatusInternalServerError, wantFiles: 1, wantTree: 1, wantRepos: []string{"MeteorsLiu/libfoo"},
			},
			{
				name: "canceled request", owner: "MeteorsLiu", index: "llarhub", cancel: true, want: http.StatusInternalServerError,
			},
			{
				name: "expired token refresh", owner: "MeteorsLiu", index: "llarhub", expireToken: true,
				pages: []string{`[{"filename":"libfoo/config"}]`}, tree: `{"tree":[{"path":"libfoo","type":"tree"}]}`,
				want: http.StatusOK, wantFiles: 1, wantTree: 1, wantRepos: []string{"MeteorsLiu/libfoo"},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var mu sync.Mutex
				var filesCalls, treeCalls, tokenCalls int
				var repos []string
				var key *rsa.PrivateKey
				var bot *Bot
				bot, key = newTestBot(t, string(secret), func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens" {
						tokenCalls++
						parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
						if len(parts) != 3 {
							t.Error("installation token request did not use an App JWT")
							w.WriteHeader(http.StatusUnauthorized)
							return
						}
						signature, err := base64.RawURLEncoding.DecodeString(parts[2])
						if err != nil {
							t.Error("invalid App JWT signature encoding")
						}
						digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
						if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
							t.Error("App JWT was not signed by the configured key")
						}
						claimsData, err := base64.RawURLEncoding.DecodeString(parts[1])
						if err != nil {
							t.Error("invalid App JWT claims encoding")
						}
						var claims struct {
							Issuer string `json:"iss"`
						}
						if err := json.Unmarshal(claimsData, &claims); err != nil || claims.Issuer != "123" {
							t.Error("App JWT did not identify the configured App")
						}
						if tc.tokenStatus != 0 {
							w.WriteHeader(tc.tokenStatus)
							io.WriteString(w, `{"message":"token rejected"}`)
							return
						}
						token := "installation-7"
						expiresAt := time.Now().Add(time.Hour)
						if tc.expireToken {
							token = fmt.Sprintf("installation-7-%d", tokenCalls)
							if tokenCalls == 1 {
								expiresAt = time.Now().Add(-time.Minute)
							}
						}
						fmt.Fprintf(w, `{"token":%q,"expires_at":%q}`, token, expiresAt.UTC().Format(time.RFC3339))
						return
					}
					wantToken := "token installation-7"
					if tc.expireToken {
						wantToken = fmt.Sprintf("token installation-7-%d", tokenCalls)
					}
					if r.Method != http.MethodGet || r.Header.Get("Authorization") != wantToken {
						t.Error("repository inspection must use authenticated GET requests")
						w.WriteHeader(http.StatusForbidden)
						return
					}
					prefix := "/repos/" + tc.owner + "/" + tc.index
					switch r.URL.Path {
					case prefix + "/pulls/3/files":
						filesCalls++
						if r.URL.Query().Get("per_page") != "100" {
							t.Error("PR files request did not set the page size")
						}
						if tc.filesStatus != 0 {
							w.WriteHeader(tc.filesStatus)
							io.WriteString(w, `{"message":"files rejected"}`)
							return
						}
						page := 1
						if value := r.URL.Query().Get("page"); value != "" {
							var err error
							page, err = strconv.Atoi(value)
							if err != nil {
								t.Error(err)
							}
						}
						if page == 2 && tc.secondPageStatus != 0 {
							w.WriteHeader(tc.secondPageStatus)
							io.WriteString(w, `{"message":"page unavailable"}`)
							return
						}
						if page < 1 || page > len(tc.pages) {
							t.Errorf("unexpected files page: %d", page)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						if page < len(tc.pages) {
							w.Header().Set("Link", fmt.Sprintf("<http://%s%s?page=%d&per_page=100>; rel=\"next\"", r.Host, r.URL.Path, page+1))
						}
						io.WriteString(w, tc.pages[page-1])
					case prefix + "/git/trees/" + mergeSHA:
						treeCalls++
						if r.URL.RawQuery != "" {
							t.Error("merge tree request should only read the root tree")
						}
						if tc.treeStatus != 0 {
							w.WriteHeader(tc.treeStatus)
							io.WriteString(w, `{"message":"tree unavailable"}`)
							return
						}
						io.WriteString(w, tc.tree)
					default:
						if !strings.HasPrefix(r.URL.Path, "/repos/"+tc.owner+"/") {
							t.Errorf("unexpected API request: %s", r.URL.Path)
							w.WriteHeader(http.StatusNotFound)
							return
						}
						repo := strings.TrimPrefix(r.URL.Path, "/repos/")
						repos = append(repos, repo)
						if tc.repoStatus != 0 {
							w.WriteHeader(tc.repoStatus)
							io.WriteString(w, `{"message":"repository rejected"}`)
							return
						}
						fmt.Fprintf(w, `{"full_name":%q}`, repo)
					}
				})
				body := fmt.Sprintf(`{"action":"closed","number":3,"pull_request":{"merged":true,"base":{"ref":"main"},"merge_commit_sha":%q},"repository":{"full_name":%q},"installation":{"id":7}}`, mergeSHA, tc.owner+"/"+tc.index)
				request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set(github.EventTypeHeader, "pull_request")
				request.Header.Set(github.SHA256SignatureHeader, signPayload(secret, []byte(body)))
				if tc.cancel {
					ctx, cancel := context.WithCancel(request.Context())
					cancel()
					request = request.WithContext(ctx)
				}
				response := httptest.NewRecorder()
				parsed, err := github.ParseWebHook("pull_request", []byte(body))
				if err != nil {
					t.Error(err)
					return
				}
				_, err = inspectProjects(request.Context(), bot.client(7), tc.owner, tc.index, parsed.(*github.PullRequestEvent))
				response.Code = http.StatusOK
				if err != nil {
					response.Code = http.StatusInternalServerError
				}
				if response.Code != tc.want {
					t.Fatalf("status = %d, want %d; body = %q", response.Code, tc.want, response.Body.String())
				}
				mu.Lock()
				defer mu.Unlock()
				wantTokens := 1
				if tc.expireToken {
					wantTokens = 2
				}
				if tc.cancel {
					wantTokens = 0
				}
				if tokenCalls != wantTokens || filesCalls != tc.wantFiles || treeCalls != tc.wantTree {
					t.Fatalf("API calls = token:%d files:%d tree:%d, want token:%d files:%d tree:%d", tokenCalls, filesCalls, treeCalls, wantTokens, tc.wantFiles, tc.wantTree)
				}
				if !slices.Equal(repos, tc.wantRepos) {
					t.Fatalf("inspected repos = %v, want %v", repos, tc.wantRepos)
				}
			})
		}
	})

	t.Run("concurrent installations", func(t *testing.T) {
		var queries atomic.Int64
		var token7, token8 atomic.Int64
		entered := make(chan string, 2)
		release := make(chan struct{})
		bot, _ := newTestBot(t, string(secret), func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost && (r.URL.Path == "/app/installations/7/access_tokens" || r.URL.Path == "/app/installations/8/access_tokens") {
				id := strings.Split(r.URL.Path, "/")[3]
				if id == "7" {
					token7.Add(1)
				} else {
					token8.Add(1)
				}
				entered <- id
				<-release
				fmt.Fprintf(w, `{"token":"installation-%s","expires_at":%q}`, id, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
				return
			}
			var owner, index, token string
			if strings.HasPrefix(r.URL.Path, "/repos/llarhub/") {
				owner, index, token = "llarhub", ".index", "installation-7"
			} else if strings.HasPrefix(r.URL.Path, "/repos/MeteorsLiu/") {
				owner, index, token = "MeteorsLiu", "llarhub", "installation-8"
			} else {
				t.Errorf("unexpected API request: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "token "+token {
				t.Error("installation tokens crossed delivery boundaries")
				w.WriteHeader(http.StatusForbidden)
				return
			}
			switch r.URL.Path {
			case "/repos/" + owner + "/" + index + "/pulls/3/files":
				io.WriteString(w, `[{"filename":"libfoo/llcppg.cfg"}]`)
			case "/repos/" + owner + "/" + index + "/git/trees/merged":
				io.WriteString(w, `{"tree":[{"path":"libfoo","type":"tree"}]}`)
			case "/repos/" + owner + "/libfoo":
				queries.Add(1)
				fmt.Fprintf(w, `{"full_name":"%s/libfoo"}`, owner)
			default:
				t.Errorf("unexpected API request: %s", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		})
		baseURL, baseClient := bot.transport.BaseURL, bot.transport.Client
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				owner, index, installation := "llarhub", ".index", 7
				if i%2 != 0 {
					owner, index, installation = "MeteorsLiu", "llarhub", 8
				}
				body := fmt.Sprintf(`{"action":"closed","number":3,"pull_request":{"merged":true,"base":{"ref":"main"},"merge_commit_sha":"merged"},"repository":{"full_name":"%s/%s"},"installation":{"id":%d}}`, owner, index, installation)
				request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set(github.EventTypeHeader, "pull_request")
				request.Header.Set(github.SHA256SignatureHeader, signPayload(secret, []byte(body)))
				response := httptest.NewRecorder()
				parsed, err := github.ParseWebHook("pull_request", []byte(body))
				if err != nil {
					t.Error(err)
					return
				}
				_, err = inspectProjects(request.Context(), bot.client(int64(installation)), owner, index, parsed.(*github.PullRequestEvent))
				response.Code = http.StatusOK
				if err != nil {
					response.Code = http.StatusInternalServerError
				}
				if response.Code != http.StatusOK {
					t.Errorf("status = %d, want OK", response.Code)
				}
			}()
		}
		var installations []string
		for range 2 {
			select {
			case id := <-entered:
				installations = append(installations, id)
			case <-time.After(5 * time.Second):
				close(release)
				wg.Wait()
				t.Fatal("installation token requests did not run independently of the cache lock")
			}
		}
		close(release)
		wg.Wait()
		if installations[0] == installations[1] {
			t.Fatal("one installation initialized or refreshed its token more than once")
		}
		if token7.Load() != 1 || token8.Load() != 1 {
			t.Fatalf("token requests = 7:%d 8:%d, want one each", token7.Load(), token8.Load())
		}
		if len(bot.clients) != 2 || bot.clients[7] == bot.clients[8] {
			t.Fatal("installation clients were not cached separately")
		}
		if bot.transport.BaseURL != baseURL || bot.transport.Client != baseClient {
			t.Fatal("installation token refresh modified the shared App transport template")
		}
		if got := queries.Load(); got != 8 {
			t.Fatalf("repository queries = %d, want 8", got)
		}
	})
	t.Run("core registration", testProvision)
	t.Run("c branch generation", testPush)
	t.Run("toc request aggregation", testTOCAggregation)
	t.Run("git authentication and push", testGitPush)
}

func newTestBot(t *testing.T, secret string, handler http.HandlerFunc) (*Bot, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "app.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	bot, err := New(secret, 123, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if bot.clients == nil || len(bot.clients) != 0 {
		t.Fatal("New must initialize an empty installation client map")
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	apiURL, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = apiURL.Hostname()
	dialer := &net.Dialer{}
	// Redirect this transport's GitHub API connections to the local TLS server;
	// production client creation and cache misses still run without test hooks.
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, server.Listener.Addr().String())
	}
	t.Cleanup(transport.CloseIdleConnections)
	bot.transport = ghinstallation.NewAppsTransportFromPrivateKey(transport, 123, key)
	return bot, key
}

func signPayload(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
