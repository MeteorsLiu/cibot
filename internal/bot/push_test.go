package bot

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-github/v83/github"
	"github.com/hibiken/asynq"
)

func testPush(t *testing.T) {
	for _, tc := range []struct {
		name, owner, index, body string
		status                   int
		wantError                bool
	}{
		{name: "unregistered production project", owner: "llarhub", index: ".index", status: http.StatusNotFound, body: `{"message":"Not Found"}`},
		{name: "unregistered test project", owner: "MeteorsLiu", index: "llarhub", status: http.StatusNotFound, body: `{"message":"Not Found"}`},
		{name: "owner without a supported index", owner: "other"},
		{name: "metadata path is a directory", owner: "llarhub", index: ".index", status: http.StatusOK, body: `[]`},
		{name: "index permission error", owner: "MeteorsLiu", index: "llarhub", status: http.StatusForbidden, body: `{"message":"Forbidden"}`, wantError: true},
		{name: "index service error", owner: "llarhub", index: ".index", status: http.StatusInternalServerError, body: `{"message":"Unavailable"}`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			invoked := filepath.Join(t.TempDir(), "invoked")
			for _, name := range []string{"git", "llcppg", "llar"} {
				script := fmt.Sprintf("#!/bin/sh\nprintf invoked > %q\nexit 97\n", invoked)
				if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			var tokenCalls, registrationCalls atomic.Int64
			bot, _, inspector := newTestBot(t, "secret", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens":
					tokenCalls.Add(1)
					fmt.Fprintf(w, `{"token":"installation-7","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
				case r.Method == http.MethodGet && r.URL.Path == "/repos/"+tc.owner+"/"+tc.index+"/contents/libfoo/versions.json":
					registrationCalls.Add(1)
					if r.URL.Query().Get("ref") != "main" || r.Header.Get("Authorization") != "token installation-7" {
						t.Error("registration lookup did not use main and the installation token")
					}
					w.WriteHeader(tc.status)
					io.WriteString(w, tc.body)
				default:
					t.Errorf("unexpected API request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			body := []byte(fmt.Sprintf(`{"ref":"refs/heads/c","after":"source-sha","repository":{"name":"libfoo","full_name":%q},"installation":{"id":7}}`, tc.owner+"/libfoo"))
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(github.EventTypeHeader, "push")
			request.Header.Set(github.SHA256SignatureHeader, signPayload([]byte("secret"), body))
			response := httptest.NewRecorder()
			bot.ServeHTTP(response, request)
			if response.Code != http.StatusAccepted {
				t.Fatalf("push status = %d, want accepted: %s", response.Code, response.Body.String())
			}
			taskID := "push:" + tc.owner + "/libfoo@source-sha"
			err := waitTask(t, inspector, taskID)
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "inspect registration "+tc.owner+"/"+tc.index+"/libfoo") {
					t.Fatalf("task error = %v, want registration lookup error", err)
				}
				info, err := inspector.GetTaskInfo(queueName, taskID)
				if err != nil {
					t.Fatal(err)
				}
				if info.State != asynq.TaskStateRetry {
					t.Fatalf("task state = %v, want retry", info.State)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			wantCalls := int64(1)
			if tc.index == "" {
				wantCalls = 0
			}
			if tokenCalls.Load() != wantCalls || registrationCalls.Load() != wantCalls {
				t.Fatalf("token calls = %d, registration calls = %d, want %d each", tokenCalls.Load(), registrationCalls.Load(), wantCalls)
			}
			if _, err := os.Stat(invoked); !os.IsNotExist(err) {
				t.Fatalf("unregistered or failed lookup ran a command: %v", err)
			}
		})
	}

	for _, tc := range []struct {
		name, owner, index, base  string
		installation              int64
		failGen, repeat, failPush bool
		changeBranch, changeAt    string
		wantError                 string
		wantState                 asynq.TaskState
	}{
		{name: "production c push", owner: "llarhub", index: ".index", base: "main", installation: 7},
		{name: "test c push", owner: "MeteorsLiu", index: "llarhub", base: "main", installation: 8},
		{name: "registered project with a different default branch", owner: "MeteorsLiu", index: "llarhub", base: "trunk", installation: 9},
		{name: "repeat delivery", owner: "MeteorsLiu", index: "llarhub", base: "main", installation: 8, repeat: true},
		{name: "failed generation", owner: "MeteorsLiu", index: "llarhub", base: "main", installation: 8, failGen: true, wantError: "llcppg: exit status 7", wantState: asynq.TaskStateRetry},
		{name: "both queued source revisions are stale", owner: "MeteorsLiu", index: "llarhub", base: "main", installation: 8, changeBranch: "c", changeAt: "queued", wantError: "c HEAD changed from", wantState: asynq.TaskStateArchived},
		{name: "source advances during generation", owner: "llarhub", index: ".index", base: "main", installation: 7, changeBranch: "c", changeAt: "generation", wantError: "HEAD changed during generation", wantState: asynq.TaskStateArchived},
		{name: "default branch advances during generation", owner: "MeteorsLiu", index: "llarhub", base: "main", installation: 8, changeBranch: "main", changeAt: "generation", wantError: "HEAD changed during generation", wantState: asynq.TaskStateArchived},
		{name: "custom default branch advances during generation", owner: "MeteorsLiu", index: "llarhub", base: "trunk", installation: 9, changeBranch: "trunk", changeAt: "generation", wantError: "HEAD changed during generation", wantState: asynq.TaskStateArchived},
		{name: "default branch advances during push", owner: "MeteorsLiu", index: "llarhub", base: "main", installation: 8, changeBranch: "main", changeAt: "push", wantError: "HEAD changed during push", wantState: asynq.TaskStateArchived},
		{name: "push hook failure with unchanged heads", owner: "MeteorsLiu", index: "llarhub", base: "main", installation: 8, failPush: true, wantError: "git push:", wantState: asynq.TaskStateRetry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			invoked := filepath.Join(t.TempDir(), "generated")
			generator := "#!/bin/sh\nset -eu\n[ -f \"$2/go.mod\" ]\n[ ! -f \"$2/versions.json\" ]\ngrep -Fq '#define LIBFOO_VALUE 1' \"$2/include/foo.h\"\ngrep -Fq '\"Name\":\"foo\"' \"$2/llcppg.cfg\"\ngrep -Fq 'link: $(pkg-config --libs foo)' \"$2/llcppg.cfg\"\nmkdir -p \"$1/sub\"\nprintf 'package foo\\n\\nconst LLGoPackage = \"link: $(pkg-config --libs foo)\"\\nconst Value = 1\\n' > \"$1/foo.go\"\nprintf 'package sub\\n\\nconst Value = 2\\n' > \"$1/sub/sub.go\"\n"
			if tc.failGen {
				generator = "#!/bin/sh\nexit 7\n"
			}
			generator += fmt.Sprintf("printf generated > %q\n", invoked)
			// A c push already carries headers. Running llar here must fail this test.
			if err := os.WriteFile(filepath.Join(bin, "llar"), []byte("#!/bin/sh\nexit 19\n"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			remote, checkout := testRepository(t, tc.base, map[string]string{"README.md": "keep documentation\n"})
			baseSHA := testGitCommand(t, remote, "rev-parse", tc.base)
			testGitCommand(t, checkout, "checkout", "-b", "c")
			sourceModule := "module github.com/" + tc.owner + "/libfoo/c\n\ngo 1.24.0\n"
			cfg := "{\"Name\":\"foo\",\"Language\":\"c\",\"Dir\":\"./include\",\"LLGoPackage\":\"link: $(pkg-config --libs foo)\"}"
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
			testGitCommand(t, checkout, "push", "origin", "c", "refs/tags/v0.1.0", "refs/tags/c/v0.1.0")
			cHead, baseHead := after, baseSHA
			revisions := []string{after}
			if tc.repeat {
				revisions = append(revisions, after)
			}
			var hook string
			if tc.changeBranch != "" {
				testGitCommand(t, checkout, "checkout", tc.changeBranch)
				testWriteFiles(t, checkout, map[string]string{"human.txt": "human change\n"})
				testGitCommand(t, checkout, "add", "--all")
				testGitCommand(t, checkout, "commit", "-m", "Human change")
				if tc.changeAt == "queued" {
					// Deliver A and B only after c has advanced to C; neither may generate.
					revisions = append(revisions, testGitCommand(t, checkout, "rev-parse", "HEAD"))
					testWriteFiles(t, checkout, map[string]string{"human.txt": "newer human change\n"})
					testGitCommand(t, checkout, "commit", "-am", "Newer human change")
				}
				changedHead := testGitCommand(t, checkout, "rev-parse", "HEAD")
				if tc.changeBranch == "c" {
					cHead = changedHead
				} else {
					baseHead = changedHead
				}
				// Explicit --git-dir also works inside a pre-push hook, where Git
				// otherwise exports the bot clone's GIT_DIR to the actor command.
				advance := fmt.Sprintf("git --git-dir=%q push origin %q\n", filepath.Join(checkout, ".git"), tc.changeBranch)
				switch tc.changeAt {
				case "queued":
					testGitCommand(t, checkout, "push", "origin", tc.changeBranch)
				case "generation":
					generator += advance
				case "push":
					hook = "#!/bin/sh\nset -eu\n" + advance
				}
			}
			if tc.failPush {
				hook = "#!/bin/sh\nexit 23\n"
			}
			if hook != "" {
				hookPath := filepath.Join(t.TempDir(), "pre-push")
				if err := os.WriteFile(hookPath, []byte(hook), 0755); err != nil {
					t.Fatal(err)
				}
				generator += fmt.Sprintf("cp %q \"$(dirname \"$2\")/repo/.git/hooks/pre-push\"\n", hookPath)
			}
			if err := os.WriteFile(filepath.Join(bin, "llcppg"), []byte(generator), 0755); err != nil {
				t.Fatal(err)
			}

			var tokenCalls, registrationCalls atomic.Int64
			bot, _, inspector := newTestBot(t, "secret", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost && r.URL.Path == fmt.Sprintf("/app/installations/%d/access_tokens", tc.installation) {
					tokenCalls.Add(1)
					fmt.Fprintf(w, "{\"token\":\"installation-%d\",\"expires_at\":%q}", tc.installation, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
					return
				}
				if r.Method == http.MethodGet && r.URL.Path == "/repos/"+tc.owner+"/"+tc.index+"/contents/libfoo/versions.json" {
					registrationCalls.Add(1)
					if r.URL.Query().Get("ref") != "main" || r.Header.Get("Authorization") != fmt.Sprintf("token installation-%d", tc.installation) {
						t.Error("registration lookup did not use main and the installation token")
					}
					io.WriteString(w, `{"type":"file","name":"versions.json","path":"libfoo/versions.json"}`)
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
			for _, revision := range revisions {
				event.After = github.Ptr(revision)
				body, err := json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set(github.EventTypeHeader, "push")
				request.Header.Set(github.SHA256SignatureHeader, signPayload([]byte("secret"), body))
				response := httptest.NewRecorder()
				bot.ServeHTTP(response, request)
				want := http.StatusAccepted
				if response.Code != want {
					t.Fatalf("push status = %d, want %d: %s", response.Code, want, response.Body.String())
				}
				taskID := "push:" + tc.owner + "/libfoo@" + revision
				err = waitTask(t, inspector, taskID)
				if tc.wantError == "" {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					if err == nil || !strings.Contains(err.Error(), tc.wantError) {
						t.Fatalf("task error = %v, want %q", err, tc.wantError)
					}
					info, err := inspector.GetTaskInfo(queueName, taskID)
					if err != nil {
						t.Fatal(err)
					}
					if info.State != tc.wantState {
						t.Fatalf("task state = %v, want %v", info.State, tc.wantState)
					}
				}
			}
			if tokenCalls.Load() != 1 {
				t.Fatalf("installation token requests = %d, want one", tokenCalls.Load())
			}
			if registrationCalls.Load() != int64(len(revisions)) {
				t.Fatalf("registration requests = %d, want %d", registrationCalls.Load(), len(revisions))
			}
			_, err := os.Stat(invoked)
			if tc.changeAt == "queued" || tc.failGen {
				if !os.IsNotExist(err) {
					t.Fatalf("stale task or failed generator produced output: %v", err)
				}
			} else if err != nil {
				t.Fatalf("generator did not produce output: %v", err)
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
			if tc.wantError != "" {
				if got := testGitCommand(t, remote, "rev-parse", tc.base); got != baseHead {
					t.Fatalf("stopped task changed the default branch: got %s, want %s", got, baseHead)
				}
				return
			}
			for name, want := range map[string]string{
				"foo.go":     "package foo\n\nconst LLGoPackage = \"link: $(pkg-config --libs foo)\"\nconst Value = 1",
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
			if parent := testGitCommand(t, remote, "rev-parse", fmt.Sprintf("%s~%d", tc.base, len(revisions))); parent != baseSHA {
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
