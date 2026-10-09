package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v83/github"
)

func testProvision(t *testing.T) {
	for _, tc := range []struct {
		name, owner, index, ownerType, base string
		existing, cBranch, failGen, repeat  bool
		failInit                            bool
		toc                                 string
	}{
		{name: "new personal repository", owner: "MeteorsLiu", index: "llarhub", ownerType: "User", base: "main"},
		{name: "new organization repository", owner: "llarhub", index: ".index", ownerType: "Organization", base: "main"},
		{name: "existing repository", owner: "MeteorsLiu", index: "llarhub", ownerType: "User", base: "main", existing: true, cBranch: true},
		{name: "existing repository without c branch", owner: "MeteorsLiu", index: "llarhub", ownerType: "User", base: "trunk", existing: true},
		{name: "append existing toc", owner: "MeteorsLiu", index: "llarhub", ownerType: "User", base: "main", toc: "old/native old\n"},
		{name: "duplicate toc mapping", owner: "MeteorsLiu", index: "llarhub", ownerType: "User", base: "main", existing: true, toc: "upstream/libfoo libfoo\n", repeat: true},
		{name: "generation failure keeps toc PR", owner: "MeteorsLiu", index: "llarhub", ownerType: "User", base: "main", failGen: true},
		{name: "initialization failure keeps toc PR", owner: "MeteorsLiu", index: "llarhub", ownerType: "User", base: "main", failInit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workRoot := t.TempDir()
			t.Setenv("TMPDIR", workRoot)
			bin := t.TempDir()
			llar := "#!/bin/sh\nset -eu\n[ \"$1\" = install ]\n[ \"$2\" = upstream/libfoo ]\n[ \"$3\" = -o ]\n[ -z \"$(printenv CIBOT_GIT_AUTH)\" ]\nmkdir -p \"$4/include\"\nprintf '#define LIBFOO_VALUE 1\\n' > \"$4/include/foo.h\"\n"
			llcppg := "#!/bin/sh\nset -eu\n[ -z \"$(printenv CIBOT_GIT_AUTH)\" ]\n"
			if tc.existing {
				llcppg += "[ \"$1\" != -init ]\n"
			}
			if tc.failInit {
				llcppg += "if [ \"$1\" = -init ]; then exit 9; fi\n"
			}
			llcppg += `if [ "$1" = -init ]; then
  [ "$2" = libfoo ]
  [ ! -f go.mod ]
  git checkout --quiet -b c
  mkdir -p c
  printf 'module github.com/llarhub/%s/c\n\ngo 1.23\n\nrequire github.com/goplus/lib v0.6.1\n' "$2" > c/go.mod
  printf 'github.com/goplus/lib v0.6.1 h1:OrHX3lBRsK3u5Vh045bkPvCoYHXhLmEqPCINHT6qA6Y=\ngithub.com/goplus/lib v0.6.1/go.mod h1:0krESx2ZyKMlgNp3oUMob++WJYR5a1PpubjSmMjnP0c=\n' > c/go.sum
  printf '{"Name":"template"}\n' > c/llcppg.cfg
  git add c
  git commit --quiet -m 'init c template'
  git checkout --quiet main
  git show c:c/go.mod | sed 's|/c$||' > go.mod
  git show c:c/go.sum > go.sum
  git add go.mod go.sum
  git commit --quiet -m 'init main template'
  exit 0
fi
`
			if tc.failGen {
				llcppg += "exit 7\n"
			}
			llcppg += "[ -f \"$2/go.mod\" ]\n[ -f \"$2/include/foo.h\" ]\ngrep -q '\"Name\":\"foo\"' \"$2/llcppg.cfg\"\ngrep -Fq '\"LLGoPackage\":\"link: $(llar install upstream/libfoo)\"' \"$2/llcppg.cfg\"\nmkdir -p \"$1\"\nprintf 'package foo\\n\\nconst LLGoPackage = \"link: $(llar install upstream/libfoo)\"\\nconst Value = 1\\n' > \"$1/foo.go\"\n"
			for name, script := range map[string]string{"llar": llar, "llcppg": llcppg} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			cfg := "{\"Name\":\"foo\",\"Language\":\"c\",\"Dir\":\"./include\",\"FuncPrefix\":[\"foo_\"],\"LLGoPackage\":\"link: -lold\"}"
			module := "module github.com/" + tc.owner + "/libfoo/c\n\ngo 1.23\n\nrequire github.com/goplus/lib v0.6.1\n"
			if tc.cBranch {
				module = "module github.com/" + tc.owner + "/libfoo/c\n\ngo 1.24.0\n"
			}
			files := map[string]string{
				"libfoo/versions.json": "{\"path\":\"upstream/libfoo\"}",
				"libfoo/llcppg.cfg":    cfg,
				"README.md":            "registry\n",
				"unused/data.txt":      "unrelated project contents\n",
			}
			if tc.toc != "" {
				files["llarhub.toc"] = tc.toc
			}
			indexRemote, indexCheckout := testRepository(t, "main", files)
			testGitCommand(t, indexRemote, "config", "uploadpack.allowFilter", "true")
			indexURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(indexRemote)}).String()
			unrelatedBlob := testGitCommand(t, indexRemote, "rev-parse", "main:unused/data.txt")
			mergeSHA := testGitCommand(t, indexRemote, "rev-parse", "main")
			// Source configuration comes from the delivered merge, not a later main.
			testWriteFiles(t, indexCheckout, map[string]string{"libfoo/llcppg.cfg": "{\"Name\":\"later\"}"})
			testGitCommand(t, indexCheckout, "commit", "-am", "Later registry change")
			testGitCommand(t, indexCheckout, "push", "origin", "main")
			indexHead := testGitCommand(t, indexRemote, "rev-parse", "main")
			projectFiles := map[string]string{"README.md": "keep project documentation\n"}
			if tc.existing {
				projectFiles["foo.go"] = "package foo\n\nconst Value = 0\n"
			}
			projectRemote, projectCheckout := testRepository(t, tc.base, projectFiles)
			mainParent := testGitCommand(t, projectRemote, "rev-parse", tc.base)
			cParent := mainParent
			if tc.cBranch {
				testGitCommand(t, projectCheckout, "checkout", "-b", "c")
				testWriteFiles(t, projectCheckout, map[string]string{"c/go.mod": module, "c/llcppg.cfg": "old configuration", "c/include/foo.h": "old header", "c/keep.txt": "keep\n"})
				testGitCommand(t, projectCheckout, "add", "--all")
				testGitCommand(t, projectCheckout, "commit", "-m", "Existing C source")
				testGitCommand(t, projectCheckout, "push", "origin", "c")
				cParent = testGitCommand(t, projectRemote, "rev-parse", "c")
			}
			if tc.existing {
				testGitCommand(t, projectCheckout, "tag", "v0.1.0", mainParent)
				testGitCommand(t, projectCheckout, "tag", "c/v0.1.0", cParent)
				testGitCommand(t, projectCheckout, "push", "origin", "refs/tags/v0.1.0", "refs/tags/c/v0.1.0")
			}
			var mu sync.Mutex
			created, pulls := 0, 0
			indexPrefix := "/repos/" + tc.owner + "/" + tc.index
			projectPrefix := "/repos/" + tc.owner + "/libfoo"
			bot, _ := newTestBot(t, "secret", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost && r.URL.Path == "/app/installations/7/access_tokens" {
					fmt.Fprintf(w, "{\"token\":\"installation-7\",\"expires_at\":%q}", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
					return
				}
				if r.Header.Get("Authorization") != "token installation-7" {
					t.Error("API request did not use the installation token")
					w.WriteHeader(http.StatusForbidden)
					return
				}
				repository := &github.Repository{Name: github.Ptr("libfoo"), FullName: github.Ptr(tc.owner + "/libfoo"), DefaultBranch: github.Ptr(tc.base), CloneURL: github.Ptr(projectRemote)}
				createPath := "/user/repos"
				if tc.ownerType == "Organization" {
					createPath = "/orgs/" + tc.owner + "/repos"
				}
				switch {
				case r.Method == http.MethodGet && r.URL.Path == indexPrefix+"/pulls/3/files":
					io.WriteString(w, "[{\"filename\":\"libfoo/llcppg.cfg\"},{\"filename\":\"libfoo/versions.json\"}]")
				case r.Method == http.MethodGet && r.URL.Path == indexPrefix+"/git/trees/"+mergeSHA:
					io.WriteString(w, "{\"tree\":[{\"path\":\"libfoo\",\"type\":\"tree\"}]}")
				case r.Method == http.MethodGet && r.URL.Path == projectPrefix:
					if !tc.existing {
						w.WriteHeader(http.StatusNotFound)
						io.WriteString(w, "{\"message\":\"Not Found\"}")
						return
					}
					json.NewEncoder(w).Encode(repository)
				case r.Method == http.MethodGet && r.URL.Path == indexPrefix+"/pulls":
					if r.URL.Query().Get("state") != "open" || r.URL.Query().Get("base") != "main" {
						t.Error("toc lookup did not filter open PRs into main")
					}
					clones, err := filepath.Glob(filepath.Join(workRoot, "cibot-merge-*", "index"))
					if err != nil || len(clones) != 1 {
						t.Errorf("expected one registry checkout, got %v: %v", clones, err)
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					config, err := os.ReadFile(filepath.Join(clones[0], "libfoo", "llcppg.cfg"))
					if err != nil || string(config) != cfg {
						t.Errorf("sparse checkout did not materialize the merged project: %q, %v", config, err)
					}
					if _, err := os.Stat(filepath.Join(clones[0], "unused")); !os.IsNotExist(err) {
						t.Errorf("unrelated project was checked out: %v", err)
					}
					missing := testGitCommand(t, clones[0], "rev-list", "--objects", "--all", "--missing=print")
					if !strings.Contains(missing, "?"+unrelatedBlob) {
						t.Error("unrelated project blob was downloaded")
					}
					io.WriteString(w, "[]")
				case r.Method == http.MethodPost && r.URL.Path == createPath:
					if pulls != 1 {
						t.Error("repository creation started before the toc PR")
					}
					var request github.Repository
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					if request.GetName() != "libfoo" || request.GetPrivate() || !request.GetAutoInit() {
						t.Error("creation did not request an initialized public project")
					}
					created++
					json.NewEncoder(w).Encode(repository)
				case r.Method == http.MethodPost && r.URL.Path == indexPrefix+"/pulls":
					var request github.NewPullRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					if request.GetHead() != "llarhub-bot/toc-pr-3" || request.GetBase() != "main" {
						t.Error("incorrect toc PR head or base")
					}
					pulls++
					io.WriteString(w, "{\"number\":4}")
				default:
					t.Errorf("unexpected API request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			event := &github.PullRequestEvent{
				Action: github.Ptr("closed"), Number: github.Ptr(3),
				PullRequest:  &github.PullRequest{Merged: github.Ptr(true), MergeCommitSHA: github.Ptr(mergeSHA), Base: &github.PullRequestBranch{Ref: github.Ptr("main")}},
				Repo:         &github.Repository{Name: github.Ptr(tc.index), FullName: github.Ptr(tc.owner + "/" + tc.index), CloneURL: github.Ptr(indexURL), Owner: &github.User{Login: github.Ptr(tc.owner), Type: github.Ptr(tc.ownerType)}},
				Installation: &github.Installation{ID: github.Ptr(int64(7))},
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
				request.Header.Set(github.EventTypeHeader, "pull_request")
				request.Header.Set(github.SHA256SignatureHeader, signPayload([]byte("secret"), body))
				response := httptest.NewRecorder()
				bot.ServeHTTP(response, request)
				want := http.StatusOK
				if tc.failGen || tc.failInit {
					want = http.StatusInternalServerError
				}
				if response.Code != want {
					t.Fatalf("status = %d, want %d: %s", response.Code, want, response.Body.String())
				}
			}
			if !bot.initMu.TryLock() {
				t.Fatal("initialization left the template cache locked")
			}
			bot.initMu.Unlock()
			mu.Lock()
			defer mu.Unlock()
			wantCreated := 1
			if tc.existing {
				wantCreated = 0
			}
			if created != wantCreated {
				t.Fatalf("created repositories = %d, want %d", created, wantCreated)
			}
			wantTOC, tocBranch := tc.toc, "main"
			if !strings.Contains(tc.toc, "upstream/libfoo libfoo\n") {
				wantTOC += "upstream/libfoo libfoo\n"
				tocBranch = "llarhub-bot/toc-pr-3"
				if pulls != 1 {
					t.Fatalf("toc PR count = %d, want 1", pulls)
				}
				if parent := testGitCommand(t, indexRemote, "rev-parse", tocBranch+"^"); parent != indexHead {
					t.Fatal("toc did not start from the latest main")
				}
				if changed := testGitCommand(t, indexRemote, "diff-tree", "--no-commit-id", "--name-only", "-r", tocBranch); changed != "llarhub.toc" {
					t.Fatalf("toc commit changed other files: %s", changed)
				}
			} else if pulls != 0 {
				t.Fatal("duplicate mapping created a PR")
			}
			if got := testGitCommand(t, indexRemote, "show", tocBranch+":llarhub.toc"); got != strings.TrimSpace(wantTOC) {
				t.Fatalf("toc = %q, want %q", got, wantTOC)
			}
			if testGitCommand(t, indexRemote, "rev-parse", "main") != indexHead {
				t.Fatal("bot changed the registry's main branch")
			}
			if tc.failGen || tc.failInit {
				if testGitCommand(t, projectRemote, "rev-parse", tc.base) != mainParent || testGitCommand(t, projectRemote, "tag", "--list") != "" {
					t.Fatal("failed generation published project commits or tags")
				}
				if testGitCommand(t, projectRemote, "branch", "--list", "c") != "" {
					t.Fatal("failed generation published a c branch")
				}
				return
			}
			for name, want := range map[string]string{
				"c:c/go.mod":           module,
				"c:c/include/foo.h":    "#define LIBFOO_VALUE 1\n",
				tc.base + ":foo.go":    "package foo\n\nconst LLGoPackage = \"link: $(llar install upstream/libfoo)\"\nconst Value = 1\n",
				tc.base + ":README.md": "keep project documentation\n",
			} {
				if got := testGitCommand(t, projectRemote, "show", name); got != strings.TrimSpace(want) {
					t.Fatalf("%s = %q, want %q", name, got, want)
				}
			}
			var gotConfig, wantConfig map[string]any
			publishedConfig := testGitCommand(t, projectRemote, "show", "c:c/llcppg.cfg")
			if err := json.Unmarshal([]byte(publishedConfig), &gotConfig); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(cfg), &wantConfig); err != nil {
				t.Fatal(err)
			}
			wantConfig["LLGoPackage"] = "link: $(llar install upstream/libfoo)"
			if !reflect.DeepEqual(gotConfig, wantConfig) {
				t.Fatalf("published configuration = %v, want %v", gotConfig, wantConfig)
			}
			if got := testGitCommand(t, projectRemote, "show", tc.base+":go.mod"); !strings.Contains(got, "module github.com/"+tc.owner+"/libfoo\n") {
				t.Fatalf("incorrect generated module: %s", got)
			}
			if tc.cBranch && testGitCommand(t, projectRemote, "show", "c:c/keep.txt") != "keep" {
				t.Fatal("publication removed unrelated C files")
			}
			commits := deliveries
			if !tc.existing {
				commits++ // llcppg -init commits the template before generated files.
				for _, branch := range []string{"c", tc.base} {
					if got := testGitCommand(t, projectRemote, "log", "-1", "--format=%an <%ae>", branch+"~1"); got != "llarhub-bot[bot] <339421020+llarhub-bot[bot]@users.noreply.github.com>" {
						t.Fatalf("unexpected template commit author: %s", got)
					}
				}
			}
			if parent := testGitCommand(t, projectRemote, "rev-parse", fmt.Sprintf("%s~%d", tc.base, commits)); parent != mainParent {
				t.Fatal("generated branch lost its history")
			}
			if parent := testGitCommand(t, projectRemote, "rev-parse", fmt.Sprintf("c~%d", commits)); parent != cParent {
				t.Fatal("c branch lost its history")
			}
			mainTag, cTag := mainParent, cParent
			if !tc.existing {
				mainTag = testGitCommand(t, projectRemote, "rev-parse", tc.base)
				cTag = testGitCommand(t, projectRemote, "rev-parse", "c")
			}
			if testGitCommand(t, projectRemote, "rev-parse", "v0.1.0") != mainTag || testGitCommand(t, projectRemote, "rev-parse", "c/v0.1.0") != cTag {
				t.Fatal("initial tags were not preserved or did not point to the published modules")
			}
			if got := testGitCommand(t, projectRemote, "log", "-1", "--format=%an <%ae>", tc.base); got != "llarhub-bot[bot] <339421020+llarhub-bot[bot]@users.noreply.github.com>" {
				t.Fatalf("unexpected commit author: %s", got)
			}
		})
	}
	t.Run("serialize init but not generation", func(t *testing.T) {
		root := t.TempDir()
		remotes := make(map[string]string)
		for _, name := range []string{"alpha", "beta"} {
			remotes[name], _ = testRepository(t, "main", map[string]string{"README.md": "project\n"})
			testWriteFiles(t, filepath.Join(root, name, "source"), map[string]string{"llcppg.cfg": `{"Name":"foo","Language":"c","Dir":"./include"}`})
		}
		bin := t.TempDir()
		llcppg := fmt.Sprintf(`#!/bin/sh
set -eu
if [ "$1" = -init ]; then
  mkdir %q
  sleep 0.1
  git checkout --quiet -b c
  mkdir c
  printf 'module github.com/llarhub/%%s/c\n\ngo 1.24.0\n' "$2" > c/go.mod
  git add c
  git commit --quiet -m 'init c'
  git checkout --quiet main
  printf 'module github.com/llarhub/%%s\n\ngo 1.24.0\n' "$2" > go.mod
  git add go.mod
  git commit --quiet -m 'init main'
  rmdir %q
  exit 0
fi
name="$(basename "$(dirname "$(dirname "$2")")")"
mkdir %q/"$name"
while [ ! -d %q ] || [ ! -d %q ]; do sleep 0.01; done
mkdir "$1"
printf 'package foo\n' > "$1/foo.go"
`, filepath.Join(root, "cache-lock"), filepath.Join(root, "cache-lock"), filepath.Join(root, "generating"), filepath.Join(root, "generating", "alpha"), filepath.Join(root, "generating", "beta"))
		if err := os.Mkdir(filepath.Join(root, "generating"), 0755); err != nil {
			t.Fatal(err)
		}
		for name, script := range map[string]string{
			"llar":   "#!/bin/sh\nset -eu\nmkdir -p \"$4/include\"\nprintf '#define VALUE 1\\n' > \"$4/include/foo.h\"\n",
			"llcppg": llcppg,
		} {
			if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		bot, _ := newTestBot(t, "secret", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/app/installations/7/access_tokens":
				fmt.Fprintf(w, "{\"token\":\"installation-7\",\"expires_at\":%q}", time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			case "/user/repos":
				var request github.Repository
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				json.NewEncoder(w).Encode(&github.Repository{Name: request.Name, DefaultBranch: github.Ptr("main"), CloneURL: github.Ptr(remotes[request.GetName()])})
			default:
				t.Errorf("unexpected request: %s", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		results := make(chan error, 2)
		for _, name := range []string{"alpha", "beta"} {
			go func() {
				err := bot.provision(ctx, bot.client(7), &github.Repository{Owner: &github.User{Login: github.Ptr("MeteorsLiu"), Type: github.Ptr("User")}}, project{name: name, source: "upstream/" + name}, filepath.Join(root, name))
				if err != nil {
					cancel()
				}
				results <- err
			}()
		}
		for range 2 {
			if err := <-results; err != nil {
				t.Error(err)
			}
		}
	})
}
