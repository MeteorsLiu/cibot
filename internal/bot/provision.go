package bot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/google/go-github/v83/github"
)

func provision(ctx context.Context, client *github.Client, indexRepo *github.Repository, project project, dir string) error {
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
	configPath := filepath.Join(sourceDir, "llcppg.cfg")
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(data, &config); err != nil {
		return err
	}
	config["LLGoPackage"], _ = json.Marshal("link: $(llar install " + project.source + ")")
	data, err = json.Marshal(config)
	if err != nil {
		return err
	}
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return err
	}
	nativeDir := filepath.Join(dir, "native")
	if err := run(ctx, sourceDir, "llar", "install", project.source, "-o", nativeDir); err != nil {
		return err
	}
	if err := copyDirectory(filepath.Join(nativeDir, "include"), filepath.Join(sourceDir, "include")); err != nil {
		return err
	}
	outputDir := filepath.Join(dir, "generated")
	if err := generate(ctx, sourceDir, outputDir, "github.com/"+owner+"/"+project.name); err != nil {
		return err
	}

	repoDir := filepath.Join(dir, "repo")
	if _, err := git(ctx, client, dir, "clone", "--quiet", repository.GetCloneURL(), repoDir); err != nil {
		return err
	}
	base := repository.GetDefaultBranch()
	cBranch, err := git(ctx, client, repoDir, "branch", "--remotes", "--list", "origin/c")
	if err != nil {
		return err
	}
	cBase := "origin/" + base
	if cBranch != "" {
		cBase = "origin/c"
	}
	if _, err := git(ctx, client, repoDir, "checkout", "--quiet", "-B", "c", cBase); err != nil {
		return err
	}
	if err := copyDirectory(sourceDir, filepath.Join(repoDir, "c")); err != nil {
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
	if isNew {
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
