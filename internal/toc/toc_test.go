package toc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestGenerate(t *testing.T) {
	t.Run("concurrent additions", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "llarhub.toc")
		original := strings.Repeat("pad/p p\n", 10000) + "old/native old"
		if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
			t.Fatal(err)
		}
		file, err := Parse(path)
		if err != nil {
			t.Fatal(err)
		}
		const projects, calls = 8, 32
		type result struct {
			project string
			added   bool
			err     error
		}
		results := make(chan result, calls)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range calls {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				project := fmt.Sprintf("proj-%02d", i%projects)
				added, err := file.Add(project, "owner/"+project)
				results <- result{project, added, err}
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		additions := make(map[string]int)
		for result := range results {
			if result.err != nil {
				t.Fatal(result.err)
			}
			if result.added {
				additions[result.project]++
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		content := string(data)
		if !strings.HasPrefix(content, original+"\n") {
			t.Fatal("concurrent additions changed existing contents or their final newline")
		}
		if lines := strings.Split(strings.TrimSuffix(strings.TrimPrefix(content, original+"\n"), "\n"), "\n"); len(lines) != projects {
			t.Fatalf("appended lines = %d, want %d", len(lines), projects)
		}
		for i := range projects {
			project := fmt.Sprintf("proj-%02d", i)
			record := "owner/" + project + " " + project + "\n"
			if additions[project] != 1 || strings.Count(content, record) != 1 {
				t.Fatalf("%s: successful appends=%d, records=%d; want one each", project, additions[project], strings.Count(content, record))
			}
		}
	})

	t.Run("unlock after failure", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "llarhub.toc")
		file, err := Parse(path)
		if err != nil {
			t.Fatal(err)
		}
		if added, err := file.Add("repo", "owner/repo"); added || !os.IsNotExist(err) {
			t.Fatalf("Add = (%v, %v), want (false, missing file error)", added, err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if added, err := file.Add("repo", "owner/repo"); err != nil || !added {
			t.Fatalf("Add after failure = (%v, %v), want (true, nil)", added, err)
		}
	})

	t.Run("append and repeat", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "llarhub.toc")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		file, err := Parse(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, entry := range [][2]string{
			{"zlib", "madler/zlib"},
			{"libpng", "libpng/libpng"},
			{"libfoo", "example.com/owner/libfoo"},
			{"libbar", "example.com/libbar"},
			{"zlib", "madler/zlib"},
		} {
			added, err := file.Add(entry[0], entry[1])
			if err != nil {
				t.Fatal(err)
			}
			if want := i < 4; added != want {
				t.Fatalf("Add(%q, %q) added = %v, want %v", entry[0], entry[1], added, want)
			}
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		want := "madler/zlib zlib\nlibpng/libpng libpng\nexample.com/owner/libfoo libfoo\nexample.com/libbar libbar\n"
		if string(got) != want {
			t.Fatalf("llarhub.toc = %q, want %q", got, want)
		}
	})

	t.Run("source without matching project", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "llarhub.toc")
		original := "owner/repo-extra extra\nother/owner/repo other\nother/source owner/repo\n"
		if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
			t.Fatal(err)
		}
		file, err := Parse(path)
		if err != nil {
			t.Fatal(err)
		}
		if added, err := file.Add("repo", "owner/repo"); err != nil || !added {
			t.Fatalf("Add = (%v, %v), want (true, nil)", added, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != original+"owner/repo repo\n" {
			t.Fatalf("unexpected contents: %q", got)
		}
	})

	for _, ending := range []string{"\n", "\r\n", ""} {
		record := "owner/boundary boundary" + ending
		for split := 0; split <= len(record); split++ {
			t.Run(fmt.Sprintf("block boundary split=%d ending=%q", split, ending), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "llarhub.toc")
				padding := "pad/" + strings.Repeat("x", 64*1024-split-len("pad/ p\n")) + " p\n"
				original := padding + record
				if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
					t.Fatal(err)
				}
				file, err := Parse(path)
				if err != nil {
					t.Fatal(err)
				}
				if added, err := file.Add("boundary", "owner/boundary"); err != nil || added {
					t.Fatalf("Add = (%v, %v), want (false, nil)", added, err)
				}
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != original {
					t.Fatal("mapping across the read boundary was appended again")
				}
			})
		}
	}

	for _, ending := range []string{"\n", "\r\n", ""} {
		t.Run(fmt.Sprintf("line ending %q", ending), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "llarhub.toc")
			original := "madler/zlib zlib" + ending
			if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}
			file, err := Parse(path)
			if err != nil {
				t.Fatal(err)
			}
			if added, err := file.Add("libpng", "libpng/libpng"); err != nil || !added {
				t.Fatalf("Add = (%v, %v), want (true, nil)", added, err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := original
			if ending == "" {
				want += "\n"
			}
			want += "libpng/libpng libpng\n"
			if string(got) != want {
				t.Fatalf("llarhub.toc = %q, want %q", got, want)
			}
		})
	}

	for _, tc := range []struct {
		name, original string
		wantAdded      bool
	}{
		{name: "source suffix", original: "example.com/owner/repo repo\n"},
		{name: "project prefix", original: "owner/repo repo-extra\n"},
		{name: "embedded record", original: "beforeowner/repo repoafter"},
		{name: "different project", original: "owner/repo other\n", wantAdded: true},
		{name: "duplicate before another mapping", original: "owner/repo repo\nowner/repo other\n"},
		{name: "literal separator", original: "owner/repo\trepo\n", wantAdded: true},
		{name: "incomplete record", original: "owner/repo\n", wantAdded: true},
		{name: "invalid record", original: "owner/repo/ proj\n", wantAdded: true},
		{name: "long line", original: strings.Repeat("x", 2*64*1024), wantAdded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "llarhub.toc")
			if err := os.WriteFile(path, []byte(tc.original), 0o644); err != nil {
				t.Fatal(err)
			}
			file, err := Parse(path)
			if err != nil {
				t.Fatalf("initialization checked contents: %v", err)
			}
			if added, err := file.Add("repo", "owner/repo"); err != nil || added != tc.wantAdded {
				t.Fatalf("Add = (%v, %v), want (%v, nil)", added, err, tc.wantAdded)
			}
			want := tc.original
			if tc.wantAdded {
				if !strings.HasSuffix(want, "\n") {
					want += "\n"
				}
				want += "owner/repo repo\n"
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != want {
				t.Fatal("unexpected contents after substring matching")
			}
		})
	}

	for _, original := range []string{
		"broken\nowner//repo proj\nowner/repo proj extra\n",
		"broken\nowner/new new\nowner//repo proj\n",
		strings.Repeat("broken\n", 20000) + "unfinished unrelated record",
	} {
		t.Run("unrelated records are not parsed", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "llarhub.toc")
			if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
				t.Fatal(err)
			}
			file, err := Parse(path)
			if err != nil {
				t.Fatal(err)
			}
			wantAdded := !strings.Contains(original, "owner/new new\n")
			if added, err := file.Add("new", "owner/new"); err != nil || added != wantAdded {
				t.Fatalf("Add = (%v, %v), want (%v, nil)", added, err, wantAdded)
			}
			want := original
			if wantAdded {
				if !strings.HasSuffix(want, "\n") {
					want += "\n"
				}
				want += "owner/new new\n"
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != want {
				t.Fatalf("unexpected appended contents: %v", err)
			}
			if added, err := file.Add("new", "owner/new"); err != nil || added {
				t.Fatalf("repeat Add = (%v, %v), want (false, nil)", added, err)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.toc")
		file, err := Parse(path)
		if err != nil {
			t.Fatalf("initialization checked existence: %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("initialization created the file: %v", err)
		}
		if added, err := file.Add("repo", "owner/repo"); !os.IsNotExist(err) || added {
			t.Fatalf("Add = (%v, %v), want (false, missing file error)", added, err)
		}
	})

	t.Run("maximum record round trip", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "llarhub.toc")
		const prefix = "pad/p p\n"
		if err := os.WriteFile(path, []byte(prefix), 0o644); err != nil {
			t.Fatal(err)
		}
		file, err := Parse(path)
		if err != nil {
			t.Fatal(err)
		}
		full := "owner/" + strings.Repeat("x", 64*1024-len("owner/")-len("p")-2)
		if added, err := file.Add("p", full); err != nil || !added {
			t.Fatalf("Add = (%v, %v), want (true, nil)", added, err)
		}
		if added, err := file.Add("p", full); err != nil || added {
			t.Fatalf("repeat Add = (%v, %v), want (false, nil)", added, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != prefix+full+" p\n" {
			t.Fatal("maximum record was changed or appended twice")
		}
	})

	for _, entry := range [][2]string{
		{"", "owner/repo"},
		{"proj extra", "owner/repo"},
		{"proj", ""},
		{"proj", "owner//repo"},
		{"proj", "owner/repo/"},
		{"proj", "plain"},
		{"", ""},
	} {
		t.Run("inputs are used as supplied", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "llarhub.toc")
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			file, err := Parse(path)
			if err != nil {
				t.Fatal(err)
			}
			if added, err := file.Add(entry[0], entry[1]); err != nil || !added {
				t.Fatalf("Add = (%v, %v), want (true, nil)", added, err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if want := entry[1] + " " + entry[0] + "\n"; string(got) != want {
				t.Fatalf("contents = %q, want %q", got, want)
			}
			if added, err := file.Add(entry[0], entry[1]); err != nil || added {
				t.Fatalf("repeat Add = (%v, %v), want (false, nil)", added, err)
			}
		})
	}

	t.Run("oversized addition leaves file unchanged", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "llarhub.toc")
		original := "madler/zlib zlib\n"
		if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
			t.Fatal(err)
		}
		file, err := Parse(path)
		if err != nil {
			t.Fatal(err)
		}
		if added, err := file.Add("proj", "owner/"+strings.Repeat("x", 64*1024)); err == nil || added {
			t.Fatalf("Add = (%v, %v), want (false, error) for an oversized mapping", added, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != original {
			t.Fatal("Add modified the file for an oversized mapping")
		}
	})
}
