package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v83/github"
)

// Bot receives GitHub webhooks. Its secret must remain unchanged while serving.
type Bot struct {
	WebhookSecret string

	transport *ghinstallation.AppsTransport
	// clientsMu protects client lookup and creation, not network requests.
	clientsMu sync.Mutex
	clients   map[int64]*github.Client
	// tocMu serializes the open-PR lookup and toc update across deliveries.
	tocMu sync.Mutex
}

var _ http.Handler = (*Bot)(nil)

// New loads the App's private key and initializes an empty client cache without
// network requests. Installation IDs come from individual webhook deliveries.
func New(webhookSecret string, appID int64, privateKeyFile string) (*Bot, error) {
	transport, err := ghinstallation.NewAppsTransportKeyFromFile(http.DefaultTransport, appID, privateKeyFile)
	if err != nil {
		return nil, err
	}
	return &Bot{
		WebhookSecret: webhookSecret,
		transport:     transport,
		clients:       make(map[int64]*github.Client),
	}, nil
}

// ServeHTTP verifies and parses POST deliveries before dispatching their events.
// The caller owns the HTTP server, routing, listening address, and shutdown.
func (b *Bot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if len(b.WebhookSecret) == 0 {
		http.Error(w, "webhook secret is required", http.StatusInternalServerError)
		return
	}

	// Verify the original bytes; parsing and re-encoding JSON changes the MAC.
	payload, err := github.ValidatePayload(r, []byte(b.WebhookSecret))
	if err != nil {
		http.Error(w, "invalid webhook signature or payload", http.StatusForbidden)
		return
	}
	event, err := github.ParseWebHook(github.WebHookType(r), payload)
	if err != nil {
		http.Error(w, "invalid webhook event", http.StatusBadRequest)
		return
	}

	delivery := github.DeliveryID(r)
	switch event := event.(type) {
	case *github.PingEvent:
		log.Printf("bot: webhook ping delivery=%s installation=%d", delivery, event.GetInstallation().GetID())
		w.WriteHeader(http.StatusOK)
		return
	case *github.PullRequestEvent:
		handled, err := b.handlePullRequest(r.Context(), delivery, event)
		if err != nil {
			log.Printf("bot: process merged PR delivery=%s: %v", delivery, err)
			http.Error(w, "failed to process merged PR", http.StatusInternalServerError)
			return
		}
		if handled {
			w.WriteHeader(http.StatusOK)
			return
		}
	case *github.PushEvent:
		handled, err := b.handlePush(r.Context(), delivery, event)
		if err != nil {
			log.Printf("bot: generate bindings delivery=%s: %v", delivery, err)
			http.Error(w, "failed to generate bindings", http.StatusInternalServerError)
			return
		}
		if handled {
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (b *Bot) client(installationID int64) *github.Client {
	b.clientsMu.Lock()
	defer b.clientsMu.Unlock()
	client := b.clients[installationID]
	if client == nil {
		// Token refresh writes AppsTransport.BaseURL and Client. Give, for example,
		// installations 7 and 8 separate copies while sharing the signer and connections.
		appTransport := *b.transport
		transport := ghinstallation.NewFromAppsTransport(&appTransport, installationID)
		client = github.NewClient(&http.Client{Transport: transport})
		b.clients[installationID] = client
	}
	return client
}

func (b *Bot) handlePullRequest(ctx context.Context, delivery string, event *github.PullRequestEvent) (bool, error) {
	if event.GetAction() != "closed" || !event.GetPullRequest().GetMerged() ||
		event.GetPullRequest().GetBase().GetRef() != "main" {
		return false, nil
	}
	repo := event.GetRepo().GetFullName()
	switch repo {
	case "llarhub/.index", "MeteorsLiu/llarhub":
	default:
		return false, nil
	}
	log.Printf("bot: index PR merged repo=%s number=%d installation=%d delivery=%s",
		repo, event.GetNumber(), event.GetInstallation().GetID(), delivery)

	client := b.client(event.GetInstallation().GetID())
	owner, index, _ := strings.Cut(repo, "/")
	projects, err := inspectProjects(ctx, client, owner, index, event)
	if err != nil || len(projects) == 0 {
		return true, err
	}
	workDir, err := os.MkdirTemp("", "cibot-merge-")
	if err != nil {
		return true, err
	}
	defer os.RemoveAll(workDir)
	indexDir := filepath.Join(workDir, "index")
	if _, err := git(ctx, client, workDir, "clone", "--quiet", "--filter=blob:none", "--no-checkout", event.GetRepo().GetCloneURL(), indexDir); err != nil {
		return true, err
	}
	args := []string{"sparse-checkout", "set", "--cone", "--"}
	for _, project := range projects {
		args = append(args, project.name)
	}
	if _, err := git(ctx, client, indexDir, args...); err != nil {
		return true, err
	}
	if _, err := git(ctx, client, indexDir, "checkout", "--quiet", "--detach", event.GetPullRequest().GetMergeCommitSHA()); err != nil {
		return true, err
	}
	for i := range projects {
		sourceDir := filepath.Join(workDir, "projects", projects[i].name, "source")
		if err := copyDirectory(filepath.Join(indexDir, projects[i].name), sourceDir); err != nil {
			return true, err
		}
		metadata, err := os.ReadFile(filepath.Join(sourceDir, "versions.json"))
		if err != nil {
			return true, err
		}
		var source struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(metadata, &source); err != nil {
			return true, err
		}
		projects[i].source = source.Path
	}
	if err := b.updateTOC(ctx, client, owner, index, event.GetNumber(), projects, indexDir); err != nil {
		return true, err
	}
	for _, project := range projects {
		if err := provision(ctx, client, event.GetRepo(), project, filepath.Join(workDir, "projects", project.name)); err != nil {
			return true, fmt.Errorf("provision %s: %w", project.name, err)
		}
	}
	return true, nil
}

type project struct {
	name       string
	source     string
	repository *github.Repository
}

func inspectProjects(ctx context.Context, client *github.Client, owner, index string, event *github.PullRequestEvent) ([]project, error) {
	var projects []string
	seen := make(map[string]bool)
	opts := &github.ListOptions{PerPage: 100}
	for {
		files, response, err := client.PullRequests.ListFiles(ctx, owner, index, event.GetNumber(), opts)
		if err != nil {
			return nil, fmt.Errorf("list PR files: %w", err)
		}
		for _, file := range files {
			// A rename can affect both its previous and current project directories.
			for _, name := range []string{file.GetFilename(), file.GetPreviousFilename()} {
				proj, _, nested := strings.Cut(name, "/")
				if !nested || strings.HasPrefix(proj, ".") || seen[proj] {
					continue
				}
				seen[proj] = true
				projects = append(projects, proj)
			}
		}
		if response.NextPage == 0 {
			break
		}
		opts.Page = response.NextPage
	}
	if len(projects) == 0 {
		return nil, nil
	}

	// Inspect this merge's tree, not the current main branch: another PR may
	// already have changed or removed a project before this delivery arrives.
	tree, _, err := client.Git.GetTree(ctx, owner, index, event.GetPullRequest().GetMergeCommitSHA(), false)
	if err != nil {
		return nil, fmt.Errorf("read merged project directories: %w", err)
	}
	if tree.GetTruncated() {
		return nil, fmt.Errorf("merged project tree was truncated")
	}
	directories := make(map[string]bool)
	for _, entry := range tree.Entries {
		if entry.GetType() == "tree" {
			directories[entry.GetPath()] = true
		}
	}
	var result []project
	for _, proj := range projects {
		if !directories[proj] {
			continue
		}
		repository, response, err := client.Repositories.Get(ctx, owner, proj)
		if err != nil {
			if response != nil && response.StatusCode == http.StatusNotFound {
				result = append(result, project{name: proj})
				continue
			}
			return nil, fmt.Errorf("inspect project repository %s/%s: %w", owner, proj, err)
		}
		result = append(result, project{name: proj, repository: repository})
	}
	return result, nil
}

func (b *Bot) handlePush(ctx context.Context, delivery string, event *github.PushEvent) (bool, error) {
	if event.GetRef() != "refs/heads/c" || event.GetDeleted() {
		return false, nil
	}
	repo := event.GetRepo()
	log.Printf("bot: c branch updated repo=%s sha=%s installation=%d delivery=%s",
		repo.GetFullName(), event.GetAfter(), event.GetInstallation().GetID(), delivery)

	client := b.client(event.GetInstallation().GetID())
	workDir, err := os.MkdirTemp("", "cibot-push-")
	if err != nil {
		return true, err
	}
	defer os.RemoveAll(workDir)
	repoDir := filepath.Join(workDir, "repo")
	if _, err := git(ctx, client, workDir, "clone", "--quiet", "--no-checkout", repo.GetCloneURL(), repoDir); err != nil {
		return true, err
	}
	if _, err := git(ctx, client, repoDir, "checkout", "--quiet", "--detach", event.GetAfter()); err != nil {
		return true, err
	}
	sourceDir := filepath.Join(workDir, "source")
	if err := copyDirectory(filepath.Join(repoDir, "c"), sourceDir); err != nil {
		return true, err
	}
	outputDir := filepath.Join(workDir, "generated")
	if err := generate(ctx, sourceDir, outputDir, "github.com/"+repo.GetFullName()); err != nil {
		return true, err
	}
	base := repo.GetDefaultBranch()
	if _, err := git(ctx, client, repoDir, "checkout", "--quiet", base); err != nil {
		return true, err
	}
	if err := copyDirectory(outputDir, repoDir); err != nil {
		return true, err
	}
	if _, err := git(ctx, client, repoDir, "add", "--all"); err != nil {
		return true, err
	}
	if _, err := git(ctx, client, repoDir, "commit", "--quiet", "--allow-empty", "-m", "Generate LLGo bindings from c@"+event.GetAfter()); err != nil {
		return true, err
	}
	_, err = git(ctx, client, repoDir, "push", "origin", "HEAD:refs/heads/"+base)
	return true, err
}
