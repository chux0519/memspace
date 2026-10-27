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

// A dotfiles repository at the home directory is not the project of every
// directory below it, only of the home directory itself. Repositories below
// home are unaffected.
func TestDetectHomeRepo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	write(t, filepath.Join(home, ".git", "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(home, ".git", "config"), "[remote \"origin\"]\n\turl = git@github.com:me/dotfiles.git\n")
	write(t, filepath.Join(home, "src", "app", ".git", "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(home, "src", "app", ".git", "config"), "[remote \"origin\"]\n\turl = git@github.com:me/app.git\n")
	notes := filepath.Join(home, "notes", "drafts")
	os.MkdirAll(notes, 0o755)

	if h := Detect(notes); h.GitIdentity != "" || h.Root != notes || h.Name != "drafts" {
		t.Errorf("below home: %+v", h)
	}
	if h := Detect(home); h.GitIdentity != "github.com/me/dotfiles" {
		t.Errorf("home itself: %+v", h)
	}
	if h := Detect(filepath.Join(home, "src", "app")); h.GitIdentity != "github.com/me/app" {
		t.Errorf("repo below home: %+v", h)
	}
}

// A directory the enclosing repository ignores is its own project, even
// when some files under it were force-added; directories the repository
// does not ignore (including new, untracked ones) belong to it.
func TestDetectIgnored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	write(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	write(t, filepath.Join(root, ".git", "config"), "[remote \"origin\"]\n\turl = git@github.com:me/umbrella.git\n")
	write(t, filepath.Join(root, ".git", "info", "exclude"), "scratch/\n")
	write(t, filepath.Join(root, ".gitignore"), "/*\n!.gitignore\n!/tools\n!docs/\nbuild/\n")
	write(t, filepath.Join(root, "tools", ".gitignore"), "vendor/**\n!vendor/keep\n")
	for _, d := range []string{"coupon/deploy", "tools/new", "tools/build", "tools/vendor/x", "docs/api", "tools/scratch"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	write(t, filepath.Join(root, "coupon", "deploy", "run.sh"), "force-added\n")

	for dir, inRepo := range map[string]bool{
		"":               true,
		"coupon":         false, // ignored by /*
		"coupon/deploy":  false, // below an ignored directory
		"tools":          true,  // re-included
		"tools/new":      true,  // untracked, not ignored
		"tools/build":    false, // unanchored pattern at any depth
		"tools/vendor/x": false,
		"docs/api":       true,
		"tools/scratch":  false, // .git/info/exclude
	} {
		h := Detect(filepath.Join(root, dir))
		if got := h.GitIdentity == "github.com/me/umbrella"; got != inRepo {
			t.Errorf("%q: in repo = %v, want %v (%+v)", dir, got, inRepo, h)
		}
		if !inRepo && h.Root != filepath.Join(root, dir) {
			t.Errorf("%q: root %q, want the directory itself", dir, h.Root)
		}
	}
}

func TestIgnorePatterns(t *testing.T) {
	cases := []struct {
		pattern, base, path string
		match               bool
	}{
		{"node_modules", "", "a/b/node_modules", true},
		{"node_modules/", "", "node_modules", true},
		{"/build", "", "build", true},
		{"/build", "", "a/build", false},
		{"a/*/c", "", "a/b/c", true},
		{"a/*/c", "", "a/b/x/c", false},
		{"a/**/c", "", "a/b/x/c", true},
		{"a/**/c", "", "a/c", true},
		{"**/logs", "", "x/logs", true},
		{"logs/**", "", "logs/x", true},
		{"tmp?", "", "tmp1", true},
		{"[ab]x", "", "bx", true},
		{"[!ab]x", "", "bx", false},
		{"out", "pkg", "pkg/out", true},
		{"out", "pkg", "out", false},
		{"/out", "pkg", "pkg/x/out", false},
		{`\#notes`, "", "#notes", true},
	}
	for _, c := range cases {
		r, ok := parseIgnore(c.pattern, c.base)
		if !ok || r.match(c.path) != c.match {
			t.Errorf("%q in %q vs %q: ok=%v match=%v, want %v", c.pattern, c.base, c.path, ok, ok && r.match(c.path), c.match)
		}
	}
	for _, line := range []string{"", "# comment", "   ", "!"} {
		if _, ok := parseIgnore(line, ""); ok {
			t.Errorf("%q parsed as a rule", line)
		}
	}
}
