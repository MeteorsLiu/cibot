package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v83/github"
	"github.com/hibiken/asynq"
)

const queueName = "cibot"

// Bot receives GitHub webhooks. Its secret must remain unchanged while serving.
type Bot struct {
	WebhookSecret string

	transport *ghinstallation.AppsTransport
	queue     *asynq.Client
	worker    *asynq.Server
	// clientsMu protects client lookup and creation, not network requests.
	clientsMu sync.Mutex
	clients   map[int64]*github.Client
	// tocMu serializes the open-PR lookup and toc update across deliveries.
	tocMu sync.Mutex
	// initMu protects llcppg's shared template cache across new repositories.
	initMu sync.Mutex
}

var _ http.Handler = (*Bot)(nil)

// New loads the App's private key and starts its private Asynq worker.
// Redis owns accepted tasks independently of webhook connections and this process.
func New(webhookSecret string, appID int64, privateKeyFile, redisAddr string) (*Bot, error) {
	transport, err := ghinstallation.NewAppsTransportKeyFromFile(http.DefaultTransport, appID, privateKeyFile)
	if err != nil {
		return nil, err
	}
	redis := asynq.RedisClientOpt{Addr: redisAddr}
	b := &Bot{
		WebhookSecret: webhookSecret,
		transport:     transport,
		clients:       make(map[int64]*github.Client),
		queue:         asynq.NewClient(redis),
		worker:        asynq.NewServer(redis, asynq.Config{Queues: map[string]int{queueName: 1}}),
	}
	if err := b.worker.Start(asynq.HandlerFunc(b.process)); err != nil {
		b.queue.Close()
		return nil, err
	}
	return b, nil
}

// ServeHTTP verifies and queues relevant POST deliveries before acknowledging them.
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
	var repo, revision string
	switch event := event.(type) {
	case *github.PingEvent:
		log.Printf("bot: webhook ping delivery=%s installation=%d", delivery, event.GetInstallation().GetID())
		w.WriteHeader(http.StatusOK)
		return
	case *github.PullRequestEvent:
		if event.GetAction() != "closed" || !event.GetPullRequest().GetMerged() ||
			event.GetPullRequest().GetBase().GetRef() != "main" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		repo = event.GetRepo().GetFullName()
		switch repo {
		case "llarhub/.index", "MeteorsLiu/llarhub":
		default:
			w.WriteHeader(http.StatusNoContent)
			return
		}
		revision = event.GetPullRequest().GetMergeCommitSHA()
	case *github.PushEvent:
		if event.GetRef() != "refs/heads/c" || event.GetDeleted() {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		repo, revision = event.GetRepo().GetFullName(), event.GetAfter()
	default:
		w.WriteHeader(http.StatusNoContent)
		return
	}
	taskID := github.WebHookType(r) + ":" + repo + "@" + revision
	task := asynq.NewTaskWithHeaders(github.WebHookType(r), payload, map[string]string{"delivery": delivery})
	_, err = b.queue.EnqueueContext(r.Context(), task,
		asynq.Queue(queueName), asynq.TaskID(taskID), asynq.Timeout(80*time.Hour))
	if err != nil && !errors.Is(err, asynq.ErrTaskIDConflict) {
		log.Printf("bot: enqueue delivery=%s: %v", delivery, err)
		http.Error(w, "failed to enqueue event", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (b *Bot) process(ctx context.Context, task *asynq.Task) error {
	event, err := github.ParseWebHook(task.Type(), task.Payload())
	if err != nil {
		return err
	}
	delivery := task.Headers()["delivery"]
	switch event := event.(type) {
	case *github.PullRequestEvent:
		_, err = b.handlePullRequest(ctx, delivery, event)
	case *github.PushEvent:
		_, err = b.handlePush(ctx, delivery, event)
	}
	if err != nil {
		log.Printf("bot: process task type=%s delivery=%s: %v", task.Type(), delivery, err)
	}
	return err
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
	repo := event.GetRepo().GetFullName()
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
		if err := b.provision(ctx, client, event.GetRepo(), project, filepath.Join(workDir, "projects", project.name)); err != nil {
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
	repo := event.GetRepo()
	owner, project, _ := strings.Cut(repo.GetFullName(), "/")
	var index string
	switch owner {
	case "llarhub":
		index = ".index"
	case "MeteorsLiu":
		index = "llarhub"
	default:
		return false, nil
	}
	client := b.client(event.GetInstallation().GetID())
	// A project's first c push can arrive before its toc PR is merged.
	// Its registration, e.g. zlib/versions.json, is already on the index's main.
	registration, _, response, err := client.Repositories.GetContents(ctx, owner, index, project+"/versions.json", &github.RepositoryContentGetOptions{Ref: "main"})
	if err != nil {
		if response != nil && response.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return true, fmt.Errorf("inspect registration %s/%s/%s: %w", owner, index, project, err)
	}
	if registration.GetType() != "file" {
		return false, nil
	}
	log.Printf("bot: c branch updated repo=%s sha=%s installation=%d delivery=%s",
		repo.GetFullName(), event.GetAfter(), event.GetInstallation().GetID(), delivery)

	workDir, err := os.MkdirTemp("", "cibot-push-")
	if err != nil {
		return true, err
	}
	defer os.RemoveAll(workDir)
	repoDir := filepath.Join(workDir, "repo")
	if _, err := git(ctx, client, workDir, "clone", "--quiet", "--no-checkout", repo.GetCloneURL(), repoDir); err != nil {
		return true, err
	}
	cHead, err := git(ctx, client, repoDir, "rev-parse", "refs/remotes/origin/c")
	if err != nil {
		return true, err
	}
	if cHead != event.GetAfter() {
		return true, fmt.Errorf("cancel c push for %s: c HEAD changed from %s to %s: %w", repo.GetFullName(), event.GetAfter(), cHead, asynq.SkipRetry)
	}
	base := repo.GetDefaultBranch()
	baseHead, err := git(ctx, client, repoDir, "rev-parse", "refs/remotes/origin/"+base)
	if err != nil {
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
	commit, err := git(ctx, client, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return true, err
	}
	heads, err := git(ctx, client, repoDir, "ls-remote", "origin", "refs/heads/c", "refs/heads/"+base)
	if err != nil {
		return true, err
	}
	var currentC, currentBase string
	for _, line := range strings.Split(heads, "\n") {
		sha, ref, _ := strings.Cut(line, "\t")
		switch ref {
		case "refs/heads/c":
			currentC = sha
		case "refs/heads/" + base:
			currentBase = sha
		}
	}
	if currentC != cHead || currentBase != baseHead {
		return true, fmt.Errorf("cancel c push for %s: c or %s HEAD changed during generation: %w", repo.GetFullName(), base, asynq.SkipRetry)
	}
	_, err = git(ctx, client, repoDir, "push", "origin", "HEAD:refs/heads/"+base)
	if err != nil {
		// A writer can advance the target after ls-remote but before our push.
		// Do not retry against that new head; unrelated push errors still retry.
		head, readErr := git(ctx, client, repoDir, "ls-remote", "origin", "refs/heads/"+base)
		if readErr == nil {
			sha, _, _ := strings.Cut(head, "\t")
			if sha != baseHead && sha != commit {
				return true, fmt.Errorf("cancel c push for %s: %s HEAD changed during push (%v): %w", repo.GetFullName(), base, err, asynq.SkipRetry)
			}
		}
	}
	return true, err
}
