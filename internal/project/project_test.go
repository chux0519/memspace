package project

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeRemote(t *testing.T) {
	want := "github.com/chux0519/memspace"
	for _, in := range []string{
		"git@github.com:chux0519/memspace.git",
		"https://github.com/chux0519/memspace",
		"https://user:tok@github.com/Chux0519/MemSpace.git",
		"ssh://git@github.com:22/chux0519/memspace.git",
		"git://github.com/chux0519/memspace/",
	} {
		if got := NormalizeRemote(in); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
	if got := NormalizeRemote("/srv/git/app.git"); got != "file:/srv/git/app.git" {
		t.Errorf("local remote: %q", got)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetectRepo(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/feature/x\n")
	write(t, filepath.Join(root, ".git", "config"), "[core]\n\tbare = false\n[remote \"upstream\"]\n\turl = git@github.com:other/fork.git\n[remote \"origin\"]\n\turl = git@github.com:Org/App.git\n")
	write(t, filepath.Join(root, ".git", "packed-refs"), "# pack-refs\nabc123 refs/heads/feature/x\n")
	sub := filepath.Join(root, "pkg", "deep")
	os.MkdirAll(sub, 0o755)

	h := Detect(sub)
	if h.Root != root || h.Name != filepath.Base(root) {
		t.Errorf("root/name = %q/%q", h.Root, h.Name)
	}
	if h.GitIdentity != "github.com/org/app" {
		t.Errorf("identity = %q", h.GitIdentity)
	}
	if h.GitBranch != "feature/x" || h.GitHead != "abc123" {
		t.Errorf("branch/head = %q/%q", h.GitBranch, h.GitHead)
	}
	if h.Key() != "git:github.com/org/app" {
		t.Errorf("key = %q", h.Key())
	}
}

func TestDetectWorktree(t *testing.T) {
	main := t.TempDir()
	write(t, filepath.Join(main, ".git", "config"), "[remote \"origin\"]\n\turl = https://example.com/a/b.git\n")
	write(t, filepath.Join(main, ".git", "refs", "heads", "wt"), "def456\n")
	wtGit := filepath.Join(main, ".git", "worktrees", "wt")
	write(t, filepath.Join(wtGit, "HEAD"), "ref: refs/heads/wt\n")
	write(t, filepath.Join(wtGit, "commondir"), "../..\n")
	wt := t.TempDir()
	write(t, filepath.Join(wt, ".git"), "gitdir: "+wtGit+"\n")

	h := Detect(wt)
	if h.Root != wt || h.GitIdentity != "example.com/a/b" || h.GitBranch != "wt" || h.GitHead != "def456" {
		t.Errorf("worktree hint = %+v", h)
	}
}

func TestDetectNoGit(t *testing.T) {
	dir := t.TempDir()
	h := Detect(dir)
	if h.Root != dir || h.GitIdentity != "" || h.Key() != "path:"+dir {
		t.Errorf("hint = %+v", h)
	}
}
