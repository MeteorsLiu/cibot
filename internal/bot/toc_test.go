package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v83/github"
	"github.com/hibiken/asynq"
)

func testTOCAggregation(t *testing.T) {
	const owner, index = "MeteorsLiu", "llarhub"
	const prefix = "/repos/" + owner + "/" + index
	remote, source := testRepository(t, "main", map[string]string{
		"llarhub.toc":     "old/native old\n",
		"README.md":       "registry\n",
		"foo/llcppg.cfg":  "merged configuration\n",
		"unused/data.txt": "unrelated project contents\n",
	})
	testGitCommand(t, remote, "config", "uploadpack.allowFilter", "true")
	remoteURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(remote)}).String()
	mergeSHA := testGitCommand(t, remote, "rev-parse", "main")
	unrelatedBlob := testGitCommand(t, remote, "rev-parse", "main:unused/data.txt")
	testWriteFiles(t, source, map[string]string{"foo/llcppg.cfg": "later configuration\n"})
	testGitCommand(t, source, "commit", "-am", "Later project update")
	testGitCommand(t, source, "push", "origin", "main")
	laterBlob := testGitCommand(t, remote, "rev-parse", "main:foo/llcppg.cfg")
	var mu sync.Mutex
	var openHead string
	var pullCount int
	bot, _, _ := newTestBot(t, "secret", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens" {
			fmt.Fprintf(w, "{\"token\":\"installation-7\",\"expires_at\":%q}", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		if r.Header.Get("Authorization") != "token installation-7" {
			t.Error("toc request did not use its installation token")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == prefix+"/pulls":
			if r.URL.Query().Get("state") != "open" || r.URL.Query().Get("base") != "main" {
				t.Error("toc lookup did not filter open PRs into main")
			}
			// An unrelated PR on page one must not hide the pending bot PR.
			if r.URL.Query().Get("page") == "" {
				w.Header().Set("Link", fmt.Sprintf("<http://%s%s?state=open&base=main&page=2&per_page=100>; rel=\"next\"", r.Host, r.URL.Path))
				io.WriteString(w, "[{\"head\":{\"ref\":\"llarhub-bot/toc-pr-1\",\"repo\":{\"full_name\":\"contributor/llarhub\"}}}]")
				return
			}
			if openHead == "" {
				io.WriteString(w, "[]")
			} else {
				pull := &github.PullRequest{Number: github.Ptr(pullCount + 1), Head: &github.PullRequestBranch{Ref: github.Ptr(openHead), Repo: &github.Repository{FullName: github.Ptr(owner + "/" + index)}}}
				json.NewEncoder(w).Encode([]*github.PullRequest{pull})
			}
		case r.Method == http.MethodGet && r.URL.Path == fmt.Sprintf("%s/pulls/%d", prefix, pullCount+1):
			io.WriteString(w, `{"state":"open","merged":false}`)
		case r.Method == http.MethodPost && r.URL.Path == prefix+"/pulls":
			var request github.NewPullRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if openHead != "" || request.GetBase() != "main" {
				t.Error("request opened another PR instead of appending to the existing one")
			}
			openHead = request.GetHead()
			pullCount++
			fmt.Fprintf(w, `{"number":%d}`, pullCount+1)
		default:
			t.Errorf("unexpected toc request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	client := bot.client(7)
	ctx := context.Background()
	for _, request := range []struct {
		number int
		name   string
	}{
		{3, "foo"},
		{5, "bar"},
		{6, "foo"},
	} {
		dir := filepath.Join(t.TempDir(), "index")
		testGitCommand(t, "", "clone", "--filter=blob:none", "--no-checkout", remoteURL, dir)
		testGitCommand(t, dir, "sparse-checkout", "set", "--cone", "foo")
		testGitCommand(t, dir, "checkout", "--detach", mergeSHA)
		if err := bot.updateTOC(ctx, client, owner, index, request.number, []project{{name: request.name, source: "upstream/" + request.name}}, dir); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "foo")); !os.IsNotExist(err) {
			t.Fatalf("toc checkout retained project files: %v", err)
		}
		missing := testGitCommand(t, dir, "rev-list", "--objects", "--all", "--missing=print")
		for _, blob := range []string{unrelatedBlob, laterBlob} {
			if !strings.Contains(missing, "?"+blob) {
				t.Fatalf("toc update downloaded an unnecessary project blob: %s", blob)
			}
		}
	}
	mu.Lock()
	head, pulls := openHead, pullCount
	mu.Unlock()
	content := testGitCommand(t, remote, "show", head+":llarhub.toc")
	if pulls != 1 || head != "llarhub-bot/toc-pr-3" || content != "old/native old\nupstream/foo foo\nupstream/bar bar" {
		t.Fatalf("aggregation: pulls=%d head=%q toc=%q", pulls, head, content)
	}
	if count := testGitCommand(t, remote, "rev-list", "--count", "main.."+head); count != "2" {
		t.Fatalf("duplicate mapping created a commit: %s", count)
	}
	if changed := testGitCommand(t, remote, "diff", "--name-only", "main", head); changed != "llarhub.toc" {
		t.Fatalf("toc aggregation changed other files: %s", changed)
	}

	// Merging the pending PR starts the next cycle from the newly updated main.
	testGitCommand(t, remote, "update-ref", "refs/heads/main", head)
	mu.Lock()
	openHead = ""
	mu.Unlock()
	dir := filepath.Join(t.TempDir(), "index")
	testGitCommand(t, "", "clone", "--filter=blob:none", "--no-checkout", remoteURL, dir)
	testGitCommand(t, dir, "sparse-checkout", "set", "--cone", "foo")
	testGitCommand(t, dir, "checkout", "--detach", mergeSHA)
	if err := bot.updateTOC(ctx, client, owner, index, 7, []project{{name: "baz", source: "upstream/baz"}}, dir); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	head, pulls = openHead, pullCount
	openHead = ""
	mu.Unlock()
	if pulls != 2 || head != "llarhub-bot/toc-pr-7" {
		t.Fatal("merged PR did not start a new toc cycle")
	}
	testGitCommand(t, remote, "update-ref", "refs/heads/main", head)

	// Both clones predate the next PR; fetching inside tocMu must see its new head.
	dirs := []string{filepath.Join(t.TempDir(), "index"), filepath.Join(t.TempDir(), "index")}
	for _, dir := range dirs {
		testGitCommand(t, "", "clone", "--filter=blob:none", "--no-checkout", remoteURL, dir)
		testGitCommand(t, dir, "sparse-checkout", "set", "--cone", "foo")
		testGitCommand(t, dir, "checkout", "--detach", mergeSHA)
	}
	var wg sync.WaitGroup
	for i, name := range []string{"qux", "corge"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := bot.updateTOC(ctx, client, owner, index, 8+i, []project{{name: name, source: "upstream/" + name}}, dirs[i]); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	head, pulls = openHead, pullCount
	mu.Unlock()
	if pulls != 3 || testGitCommand(t, remote, "rev-list", "--count", "main.."+head) != "2" {
		t.Fatalf("concurrent requests did not share one PR: pulls=%d", pulls)
	}
	content = testGitCommand(t, remote, "show", head+":llarhub.toc")
	for _, record := range []string{"old/native old", "upstream/foo foo", "upstream/bar bar", "upstream/baz baz", "upstream/qux qux", "upstream/corge corge"} {
		if strings.Count(content+"\n", record+"\n") != 1 {
			t.Fatalf("toc lost or duplicated %q: %q", record, content)
		}
	}

	for _, tc := range []struct {
		name                          string
		merged, includesUpdate        bool
		alreadyAdded, lookupFailsOnce bool
	}{
		{name: "merged before push", merged: true},
		{name: "merged with the pushed update", merged: true, includesUpdate: true},
		{name: "duplicate still checks PR state", merged: true, alreadyAdded: true},
		{name: "closed without merging"},
		{name: "state lookup fails after push", lookupFailsOnce: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const oldBranch = "llarhub-bot/toc-pr-3"
			const baseTOC = "old/native old\n"
			const pendingTOC = baseTOC + "upstream/foo foo\n"
			const updatedTOC = pendingTOC + "upstream/bar bar\n"
			remote, source := testRepository(t, "main", map[string]string{"llarhub.toc": baseTOC, "README.md": "keep\n"})
			initialMain := testGitCommand(t, remote, "rev-parse", "main")
			testGitCommand(t, source, "checkout", "-b", oldBranch)
			testWriteFiles(t, source, map[string]string{"llarhub.toc": pendingTOC})
			testGitCommand(t, source, "commit", "-am", "Existing pending mapping")
			beforeAddition := testGitCommand(t, source, "rev-parse", "HEAD")
			if tc.alreadyAdded {
				testWriteFiles(t, source, map[string]string{"llarhub.toc": updatedTOC})
				testGitCommand(t, source, "commit", "-am", "Earlier delivery added the mapping")
			}
			testGitCommand(t, source, "push", "origin", oldBranch)
			branchBefore := testGitCommand(t, remote, "rev-parse", oldBranch)

			var stateMu sync.Mutex
			var closed bool
			var stateReads, created int
			var newHead string
			bot, _, _ := newTestBot(t, "secret", func(w http.ResponseWriter, r *http.Request) {
				stateMu.Lock()
				defer stateMu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens":
					fmt.Fprintf(w, `{"token":"installation-7","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
				case r.Method == http.MethodGet && r.URL.Path == prefix+"/pulls":
					if closed {
						io.WriteString(w, `[]`)
						return
					}
					fmt.Fprintf(w, `[{"number":7,"head":{"ref":%q,"repo":{"full_name":%q}}}]`, oldBranch, owner+"/"+index)
					if tc.merged && !tc.includesUpdate {
						// The list response is already stale when the caller fetches
						// this branch: main received only the earlier PR contents.
						testGitCommand(t, remote, "update-ref", "refs/heads/main", beforeAddition)
						closed = true
					}
				case r.Method == http.MethodGet && r.URL.Path == prefix+"/pulls/7":
					stateReads++
					if got := testGitCommand(t, remote, "show", oldBranch+":llarhub.toc"); got != strings.TrimSpace(updatedTOC) {
						t.Errorf("PR state was checked before the branch contained the update: %q", got)
					}
					if tc.lookupFailsOnce {
						if stateReads == 1 {
							w.WriteHeader(http.StatusInternalServerError)
							io.WriteString(w, `{"message":"state lookup unavailable"}`)
						} else {
							io.WriteString(w, `{"state":"open","merged":false}`)
						}
						return
					}
					if tc.includesUpdate {
						testGitCommand(t, remote, "update-ref", "refs/heads/main", oldBranch)
					}
					closed = true
					fmt.Fprintf(w, `{"state":"closed","merged":%t}`, tc.merged)
				case r.Method == http.MethodPost && r.URL.Path == prefix+"/pulls":
					var request github.NewPullRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					newHead = request.GetHead()
					created++
					if newHead == oldBranch || request.GetBase() != "main" {
						t.Error("retry reused the merged PR branch or changed the base")
					}
					io.WriteString(w, `{"number":10}`)
				default:
					t.Errorf("unexpected API request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			client := bot.client(7)
			projects := []project{{name: "bar", source: "upstream/bar"}}
			dir := filepath.Join(t.TempDir(), "index")
			testGitCommand(t, "", "clone", "--no-checkout", remote, dir)
			err := bot.updateTOC(ctx, client, owner, index, 9, projects, dir)
			if err == nil {
				t.Fatal("closed PR or failed state lookup was reported as success")
			}
			if !tc.merged && !tc.lookupFailsOnce {
				if !errors.Is(err, asynq.SkipRetry) {
					t.Fatalf("manual close must stop automatic retries: %v", err)
				}
				if testGitCommand(t, remote, "rev-parse", "main") != initialMain {
					t.Fatal("manual close changed main")
				}
				stateMu.Lock()
				defer stateMu.Unlock()
				if created != 0 || stateReads != 1 {
					t.Fatalf("manual close: created=%d state reads=%d", created, stateReads)
				}
				return
			}
			if errors.Is(err, asynq.SkipRetry) {
				t.Fatalf("merged PR or failed read must remain retryable: %v", err)
			}
			if tc.merged && !strings.Contains(err.Error(), "merged during update") {
				t.Fatalf("expected merged PR error, got %v", err)
			}
			branchAfter := testGitCommand(t, remote, "rev-parse", oldBranch)
			if tc.alreadyAdded && branchAfter != branchBefore {
				t.Fatal("duplicate mapping created another commit")
			}

			// Simulate the next Asynq attempt with a fresh clone, as the PR handler does.
			retryDir := filepath.Join(t.TempDir(), "index")
			testGitCommand(t, "", "clone", "--no-checkout", remote, retryDir)
			if err := bot.updateTOC(ctx, client, owner, index, 9, projects, retryDir); err != nil {
				t.Fatal(err)
			}
			stateMu.Lock()
			defer stateMu.Unlock()
			switch {
			case tc.lookupFailsOnce:
				if created != 0 || stateReads != 2 || testGitCommand(t, remote, "rev-parse", oldBranch) != branchAfter {
					t.Fatalf("lookup retry changed the branch or opened a PR: created=%d state reads=%d", created, stateReads)
				}
			case tc.includesUpdate:
				if created != 0 || stateReads != 1 || testGitCommand(t, remote, "show", "main:llarhub.toc") != strings.TrimSpace(updatedTOC) {
					t.Fatal("already merged mappings were resubmitted")
				}
			default:
				if created != 1 || stateReads != 1 || newHead != "llarhub-bot/toc-pr-9" {
					t.Fatalf("missing mappings did not get one new PR: created=%d state reads=%d head=%q", created, stateReads, newHead)
				}
				if testGitCommand(t, remote, "rev-parse", newHead+"^") != testGitCommand(t, remote, "rev-parse", "main") {
					t.Fatal("retry did not start from latest main")
				}
				if got := testGitCommand(t, remote, "show", newHead+":llarhub.toc"); got != strings.TrimSpace(updatedTOC) {
					t.Fatalf("retry lost or duplicated records: %q", got)
				}
				if changed := testGitCommand(t, remote, "diff", "--name-only", "main", newHead); changed != "llarhub.toc" {
					t.Fatalf("retry modified unrelated files: %s", changed)
				}
			}
		})
	}
}
