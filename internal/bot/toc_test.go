package bot

import (
	"context"
	"encoding/json"
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
				pull := &github.PullRequest{Head: &github.PullRequestBranch{Ref: github.Ptr(openHead), Repo: &github.Repository{FullName: github.Ptr(owner + "/" + index)}}}
				json.NewEncoder(w).Encode([]*github.PullRequest{pull})
			}
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
			io.WriteString(w, "{\"number\":2}")
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
}
