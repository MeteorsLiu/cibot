package bot

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Local bare repositories exercise Git's actual commits, refs, and push rules.
func testRepository(t *testing.T, branch string, files map[string]string) (remote, checkout string) {
	t.Helper()
	root := t.TempDir()
	remote, checkout = filepath.Join(root, "remote.git"), filepath.Join(root, "checkout")
	testGitCommand(t, root, "init", "--bare", "--initial-branch="+branch, remote)
	testGitCommand(t, root, "init", "--initial-branch="+branch, checkout)
	testWriteFiles(t, checkout, files)
	testGitCommand(t, checkout, "add", "--all")
	testGitCommand(t, checkout, "commit", "--allow-empty", "-m", "Initial repository")
	testGitCommand(t, checkout, "remote", "add", "origin", remote)
	testGitCommand(t, checkout, "push", "origin", branch)
	return remote, checkout
}

func testWriteFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		file := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func testGitCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	options := []string{"-c", "user.name=Test", "-c", "user.email=test@example.com"}
	cmd := exec.Command("git", append(options, args...)...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, &stderr)
	}
	return strings.TrimSpace(string(output))
}

func testGitPush(t *testing.T) {
	remote, first := testRepository(t, "main", map[string]string{"README.md": "initial\n"})
	second := filepath.Join(t.TempDir(), "second")
	testGitCommand(t, "", "clone", remote, second)
	testWriteFiles(t, first, map[string]string{"README.md": "first publisher\n"})
	testGitCommand(t, first, "commit", "-am", "First publisher")
	testGitCommand(t, first, "push", "origin", "main")
	published := testGitCommand(t, remote, "rev-parse", "main")

	testWriteFiles(t, second, map[string]string{"README.md": "stale publisher\n"})
	testGitCommand(t, second, "commit", "-am", "Stale publisher")
	bot, _, _ := newTestBot(t, "secret", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || (r.URL.Path != "/app/installations/7/access_tokens" && r.URL.Path != "/app/installations/8/access_tokens") {
			t.Errorf("unexpected API request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		id := strings.Split(r.URL.Path, "/")[3]
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "{\"token\":\"installation-%s\",\"expires_at\":%q}", id, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	})
	client := bot.client(7)
	if _, err := git(context.Background(), client, second, "push", "origin", "HEAD:refs/heads/main"); err == nil {
		t.Fatal("stale publication overwrote the remote branch")
	}
	if got := testGitCommand(t, remote, "rev-parse", "main"); got != published {
		t.Fatal("rejected push changed the remote head")
	}
	for _, id := range []int64{7, 8} {
		client := bot.client(id)
		got, err := git(context.Background(), client, second, "config", "--get-urlmatch", "http.extraHeader", "https://github.com/MeteorsLiu/cibot")
		if err != nil {
			t.Fatal(err)
		}
		want := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("x-access-token:installation-%d", id)))
		if got != want {
			t.Fatal("Git did not receive the matching installation token")
		}
	}
	config := testGitCommand(t, second, "config", "--local", "--list")
	if strings.Contains(config, "installation-") || strings.Contains(strings.ToLower(config), "extraheader") {
		t.Fatal("Git persisted its installation credentials")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := git(ctx, client, second, "status", "--short"); err == nil {
		t.Fatal("Git command ignored context cancellation")
	}
}
