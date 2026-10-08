package bot

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-github/v83/github"
)

func testPush(t *testing.T) {
	for _, tc := range []struct {
		name, owner, base string
		installation      int64
		failGen, repeat   bool
	}{
		{name: "production c push", owner: "llarhub", base: "main", installation: 7},
		{name: "test c push", owner: "MeteorsLiu", base: "main", installation: 8},
		{name: "other owner and default branch", owner: "other", base: "trunk", installation: 9},
		{name: "repeat delivery", owner: "MeteorsLiu", base: "main", installation: 8, repeat: true},
		{name: "failed generation", owner: "MeteorsLiu", base: "main", installation: 8, failGen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			generator := "#!/bin/sh\nset -eu\n[ -f \"$2/go.mod\" ]\n[ ! -f \"$2/versions.json\" ]\ngrep -Fq '#define LIBFOO_VALUE 1' \"$2/include/foo.h\"\ngrep -Fq '\"Name\":\"foo\"' \"$2/llcppg.cfg\"\ngrep -Fq 'link: $(llar install upstream/libfoo)' \"$2/llcppg.cfg\"\nmkdir -p \"$1/sub\"\nprintf 'package foo\\n\\nconst LLGoPackage = \"link: $(llar install upstream/libfoo)\"\\nconst Value = 1\\n' > \"$1/foo.go\"\nprintf 'package sub\\n\\nconst Value = 2\\n' > \"$1/sub/sub.go\"\n"
			if tc.failGen {
				generator = "#!/bin/sh\nexit 7\n"
			}
			if err := os.WriteFile(filepath.Join(bin, "llcppg"), []byte(generator), 0755); err != nil {
				t.Fatal(err)
			}
			// A c push already carries headers. Running llar here must fail this test.
			if err := os.WriteFile(filepath.Join(bin, "llar"), []byte("#!/bin/sh\nexit 19\n"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			remote, checkout := testRepository(t, tc.base, map[string]string{"README.md": "keep documentation\n"})
			baseSHA := testGitCommand(t, remote, "rev-parse", tc.base)
			testGitCommand(t, checkout, "checkout", "-b", "c")
			sourceModule := "module github.com/" + tc.owner + "/libfoo/c\n\ngo 1.24.0\n"
			cfg := "{\"Name\":\"foo\",\"Language\":\"c\",\"Dir\":\"./include\",\"LLGoPackage\":\"link: $(llar install upstream/libfoo)\"}"
			testWriteFiles(t, checkout, map[string]string{
				"c/go.mod":        sourceModule,
				"c/llcppg.cfg":    cfg,
				"c/include/foo.h": "#define LIBFOO_VALUE 1\n",
			})
			testGitCommand(t, checkout, "add", "--all")
			testGitCommand(t, checkout, "commit", "-m", "C source for this delivery")
			after := testGitCommand(t, checkout, "rev-parse", "HEAD")
			testGitCommand(t, checkout, "tag", "v0.1.0", baseSHA)
			testGitCommand(t, checkout, "tag", "c/v0.1.0", after)

			// This event must use its after SHA even if the remote c branch advances.
			testWriteFiles(t, checkout, map[string]string{
				"c/llcppg.cfg":    strings.Replace(cfg, "\"foo\"", "\"later\"", 1),
				"c/include/foo.h": "#define LIBFOO_VALUE 2\n",
			})
			testGitCommand(t, checkout, "commit", "-am", "Later C source")
			testGitCommand(t, checkout, "push", "origin", "c", "refs/tags/v0.1.0", "refs/tags/c/v0.1.0")
			cHead := testGitCommand(t, remote, "rev-parse", "c")

			var tokenCalls atomic.Int64
			bot, _ := newTestBot(t, "secret", func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == fmt.Sprintf("/app/installations/%d/access_tokens", tc.installation) {
					tokenCalls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, "{\"token\":\"installation-%d\",\"expires_at\":%q}", tc.installation, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
					return
				}
				t.Errorf("c push unexpectedly called GitHub API: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			})
			event := &github.PushEvent{
				Ref: github.Ptr("refs/heads/c"), After: github.Ptr(after),
				Repo: &github.PushEventRepository{
					Name: github.Ptr("libfoo"), FullName: github.Ptr(tc.owner + "/libfoo"),
					DefaultBranch: github.Ptr(tc.base), CloneURL: github.Ptr(remote),
				},
				Installation: &github.Installation{ID: github.Ptr(tc.installation)},
			}
			body, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			deliveries := 1
			if tc.repeat {
				deliveries = 2
			}
			for range deliveries {
				request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set(github.EventTypeHeader, "push")
				request.Header.Set(github.SHA256SignatureHeader, signPayload([]byte("secret"), body))
				response := httptest.NewRecorder()
				bot.ServeHTTP(response, request)
				want := http.StatusOK
				if tc.failGen {
					want = http.StatusInternalServerError
				}
				if response.Code != want {
					t.Fatalf("push status = %d, want %d: %s", response.Code, want, response.Body.String())
				}
			}
			if tokenCalls.Load() != 1 {
				t.Fatalf("installation token requests = %d, want one", tokenCalls.Load())
			}
			if got := testGitCommand(t, remote, "rev-parse", "c"); got != cHead {
				t.Fatal("binding generation changed the source branch")
			}
			if testGitCommand(t, remote, "rev-parse", "v0.1.0") != baseSHA || testGitCommand(t, remote, "rev-parse", "c/v0.1.0") != after {
				t.Fatal("c push moved an existing version tag")
			}
			if tags := testGitCommand(t, remote, "tag", "--list"); tags != "c/v0.1.0\nv0.1.0" {
				t.Fatalf("c push created unexpected tags: %s", tags)
			}
			if tc.failGen {
				if got := testGitCommand(t, remote, "rev-parse", tc.base); got != baseSHA {
					t.Fatal("failed generation changed the default branch")
				}
				return
			}
			for name, want := range map[string]string{
				"foo.go":     "package foo\n\nconst LLGoPackage = \"link: $(llar install upstream/libfoo)\"\nconst Value = 1",
				"sub/sub.go": "package sub\n\nconst Value = 2",
				"README.md":  "keep documentation",
			} {
				if got := testGitCommand(t, remote, "show", tc.base+":"+name); got != want {
					t.Fatalf("%s = %q, want %q", name, got, want)
				}
			}
			if module := testGitCommand(t, remote, "show", tc.base+":go.mod"); !strings.Contains(module, "module github.com/"+tc.owner+"/libfoo\n") {
				t.Fatalf("incorrect generated module: %s", module)
			}
			if parent := testGitCommand(t, remote, "rev-parse", fmt.Sprintf("%s~%d", tc.base, deliveries)); parent != baseSHA {
				t.Fatal("default branch lost its history")
			}
			if message := testGitCommand(t, remote, "log", "-1", "--format=%s", tc.base); !strings.Contains(message, after) {
				t.Fatal("generation commit does not identify the source commit")
			}
			changed := testGitCommand(t, remote, "diff", "--name-only", baseSHA, tc.base)
			if changed != "foo.go\ngo.mod\nsub/sub.go" {
				t.Fatalf("generation changed unexpected files: %s", changed)
			}
		})
	}
}
