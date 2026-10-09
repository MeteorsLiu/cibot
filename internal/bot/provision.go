package bot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/google/go-github/v83/github"
)

func (b *Bot) provision(ctx context.Context, client *github.Client, indexRepo *github.Repository, project project, dir string) error {
	owner := indexRepo.GetOwner().GetLogin()
	repository := project.repository
	isNew := repository == nil
	if isNew {
		org := ""
		if indexRepo.GetOwner().GetType() == "Organization" {
			org = owner
		}
		var err error
		repository, _, err = client.Repositories.Create(ctx, org, &github.Repository{
			Name: github.Ptr(project.name), Private: github.Ptr(false), AutoInit: github.Ptr(true),
		})
		if err != nil {
			return err
		}
	}

	sourceDir := filepath.Join(dir, "source")
	nativeDir := filepath.Join(dir, "native")
	if err := run(ctx, sourceDir, "llar", "install", project.source, "-o", nativeDir); err != nil {
		return err
	}
	if err := copyDirectory(filepath.Join(nativeDir, "include"), filepath.Join(sourceDir, "include")); err != nil {
		return err
	}
	repoDir := filepath.Join(dir, "repo")
	if _, err := git(ctx, client, dir, "clone", "--quiet", repository.GetCloneURL(), repoDir); err != nil {
		return err
	}
	base := repository.GetDefaultBranch()
	if isNew {
		command := exec.CommandContext(ctx, "llcppg", "-init", project.name)
		command.Dir = repoDir
		command.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=llarhub-bot[bot]",
			"GIT_AUTHOR_EMAIL=339421020+llarhub-bot[bot]@users.noreply.github.com",
			"GIT_COMMITTER_NAME=llarhub-bot[bot]",
			"GIT_COMMITTER_EMAIL=339421020+llarhub-bot[bot]@users.noreply.github.com",
		)
		b.initMu.Lock()
		output, err := command.CombinedOutput()
		b.initMu.Unlock()
		if err != nil {
			return fmt.Errorf("llcppg -init: %w\n%s", err, output)
		}
	}
	cBranch, err := git(ctx, client, repoDir, "branch", "--remotes", "--list", "origin/c")
	if err != nil {
		return err
	}
	cBase := "origin/" + base
	if isNew {
		cBase = "c"
	} else if cBranch != "" {
		cBase = "origin/c"
	}
	if _, err := git(ctx, client, repoDir, "checkout", "--quiet", "-B", "c", cBase); err != nil {
		return err
	}
	cDir := filepath.Join(repoDir, "c")
	if err := os.MkdirAll(cDir, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(sourceDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		switch name := entry.Name(); name {
		case "llcppg.cfg", "include", "go.mod", "go.sum":
			if err := copyDirectory(filepath.Join(sourceDir, name), filepath.Join(cDir, name)); err != nil {
				return err
			}
		}
	}
	modulePath := "github.com/" + owner + "/" + project.name
	if !isNew && cBranch == "" {
		if err := run(ctx, cDir, "go", "mod", "init", modulePath+"/c"); err != nil {
			return err
		}
		if err := run(ctx, cDir, "go", "mod", "edit", "-go=1.23", "-require=github.com/goplus/lib@v0.6.1"); err != nil {
			return err
		}
		if err := run(ctx, cDir, "go", "mod", "download", "github.com/goplus/lib@v0.6.1"); err != nil {
			return err
		}
	}
	if err := run(ctx, cDir, "go", "mod", "edit", "-module", modulePath+"/c"); err != nil {
		return err
	}
	if err := run(ctx, cDir, "go", "mod", "download"); err != nil {
		return err
	}
	outputDir := filepath.Join(dir, "generated")
	if err := generate(ctx, cDir, outputDir, modulePath); err != nil {
		return err
	}
	if _, err := git(ctx, client, repoDir, "add", "--all", "--", "c"); err != nil {
		return err
	}
	if _, err := git(ctx, client, repoDir, "commit", "--quiet", "--allow-empty", "-m", "Update C source for llcppg"); err != nil {
		return err
	}
	if _, err := git(ctx, client, repoDir, "checkout", "--quiet", base); err != nil {
		return err
	}
	if err := copyDirectory(outputDir, repoDir); err != nil {
		return err
	}
	if _, err := git(ctx, client, repoDir, "add", "--all"); err != nil {
		return err
	}
	if _, err := git(ctx, client, repoDir, "commit", "--quiet", "--allow-empty", "-m", "Generate LLGo bindings with llcppg"); err != nil {
		return err
	}
	refs := []string{"refs/heads/c:refs/heads/c", "refs/heads/" + base + ":refs/heads/" + base}
	tags, err := git(ctx, client, repoDir, "tag", "--list", "v0.1.0", "c/v0.1.0")
	if err != nil {
		return err
	}
	if tags == "" {
		if _, err := git(ctx, client, repoDir, "tag", "c/v0.1.0", "c"); err != nil {
			return err
		}
		if _, err := git(ctx, client, repoDir, "tag", "v0.1.0", base); err != nil {
			return err
		}
		refs = append(refs, "refs/tags/c/v0.1.0", "refs/tags/v0.1.0")
	}
	_, err = git(ctx, client, repoDir, append([]string{"push", "origin"}, refs...)...)
	return err
}

func generate(ctx context.Context, sourceDir, outputDir, modulePath string) error {
	if err := run(ctx, sourceDir, "llcppg", outputDir, sourceDir); err != nil {
		return err
	}
	module, err := os.ReadFile(filepath.Join(sourceDir, "go.mod"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outputDir, "go.mod"), module, 0644); err != nil {
		return err
	}
	if err := run(ctx, outputDir, "go", "mod", "edit", "-module", modulePath); err != nil {
		return err
	}
	return run(ctx, outputDir, "go", "mod", "tidy")
}
