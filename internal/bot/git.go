package bot

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v83/github"
)

func git(ctx context.Context, client *github.Client, dir string, args ...string) (string, error) {
	transport := client.Client().Transport.(*ghinstallation.Transport)
	token, err := transport.Token(ctx)
	if err != nil {
		return "", err
	}
	options := []string{
		"--config-env=http.https://github.com/.extraHeader=CIBOT_GIT_AUTH",
		"-c", "credential.helper=",
		"-c", "user.name=llarhub-bot[bot]",
		"-c", "user.email=339421020+llarhub-bot[bot]@users.noreply.github.com",
	}
	command := exec.CommandContext(ctx, "git", append(options, args...)...)
	command.Dir = dir
	// Keep installation credentials out of argv and the clone's .git/config.
	command.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"CIBOT_GIT_AUTH=AUTHORIZATION: basic "+base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token)),
	)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", args[0], err, &stderr)
	}
	return strings.TrimSpace(string(output)), nil
}

// copyDirectory overlays files while retaining unrelated files in the destination.
func copyDirectory(source, dest string) error {
	return filepath.WalkDir(source, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, file)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		input, err := os.Open(file)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			return err
		}
		_, err = io.Copy(output, input)
		closeErr := output.Close()
		if err != nil {
			return err
		}
		return closeErr
	})
}

func run(ctx context.Context, dir, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", name, err, output)
	}
	return nil
}
