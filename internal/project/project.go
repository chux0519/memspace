// Package project derives a path-independent project identity from a working
// directory: the normalized Git remote when there is one, otherwise
// the directory name. It reads .git directly instead of shelling out to git,
// because it runs inside agent hooks where latency matters.
package project

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Hint describes the project an event was observed in. It is resolved to a
// server project_id at upload time, so hooks work offline.
type Hint struct {
	// Root is the absolute repository root (or the cwd outside Git).
	Root string `json:"root"`
	// Name is a human-readable project name (repository directory name).
	Name string `json:"name"`
	// GitIdentity is the normalized origin remote, e.g. "github.com/org/repo".
	GitIdentity string `json:"git_identity,omitempty"`
	GitBranch   string `json:"git_branch,omitempty"`
	GitHead     string `json:"git_head,omitempty"`
}

// Key identifies the project for client-side caching.
func (h Hint) Key() string {
	if h.GitIdentity != "" {
		return "git:" + h.GitIdentity
	}
	return "path:" + h.Root
}

// Detect inspects cwd. It never fails: without Git it falls back to cwd.
func Detect(cwd string) Hint {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	root, gitDir, commonDir := findGit(cwd)
	if root == "" {
		return Hint{Root: cwd, Name: filepath.Base(cwd)}
	}
	h := Hint{Root: root, Name: filepath.Base(root)}
	h.GitBranch, h.GitHead = readHead(gitDir, commonDir)
	if url := originURL(filepath.Join(commonDir, "config")); url != "" {
		h.GitIdentity = NormalizeRemote(url)
	}
	return h
}

// findGit walks up from dir to the nearest .git (directory, or a file pointing
// at a worktree gitdir). Returns the work tree root, the git dir and the
// common dir (shared config/refs for worktrees).
//
// A repository at the home directory (dotfiles) only counts when dir is the
// home directory itself: the walk does not climb into it from below, or
// every directory outside a repository would belong to the dotfiles.
func findGit(dir string) (root, gitDir, commonDir string) {
	home, _ := os.UserHomeDir()
	if home != "" {
		home = filepath.Clean(home)
	}
	for d := dir; ; d = filepath.Dir(d) {
		if d == home && d != dir {
			return "", "", ""
		}
		p := filepath.Join(d, ".git")
		if fi, err := os.Stat(p); err == nil {
			if fi.IsDir() {
				return d, p, p
			}
			if b, err := os.ReadFile(p); err == nil {
				line := strings.TrimSpace(string(b))
				if rest, ok := strings.CutPrefix(line, "gitdir:"); ok {
					gd := strings.TrimSpace(rest)
					if !filepath.IsAbs(gd) {
						gd = filepath.Join(d, gd)
					}
					cd := gd
					if c, err := os.ReadFile(filepath.Join(gd, "commondir")); err == nil {
						cd = strings.TrimSpace(string(c))
						if !filepath.IsAbs(cd) {
							cd = filepath.Join(gd, cd)
						}
					}
					return d, gd, filepath.Clean(cd)
				}
			}
		}
		if parent := filepath.Dir(d); parent == d {
			return "", "", ""
		}
	}
}

func readHead(gitDir, commonDir string) (branch, head string) {
	b, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", ""
	}
	s := strings.TrimSpace(string(b))
	ref, ok := strings.CutPrefix(s, "ref: ")
	if !ok {
		return "", s // detached HEAD
	}
	branch = strings.TrimPrefix(ref, "refs/heads/")
	for _, d := range []string{gitDir, commonDir} {
		if b, err := os.ReadFile(filepath.Join(d, filepath.FromSlash(ref))); err == nil {
			return branch, strings.TrimSpace(string(b))
		}
	}
	if f, err := os.Open(filepath.Join(commonDir, "packed-refs")); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if sha, name, ok := strings.Cut(sc.Text(), " "); ok && name == ref {
				return branch, sha
			}
		}
	}
	return branch, "" // unborn branch
}

// originURL returns remote "origin" (or the first remote) from a git config.
func originURL(configPath string) string {
	f, err := os.Open(configPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	var section, first, origin string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "url" || !strings.HasPrefix(section, `[remote "`) {
			continue
		}
		v = strings.TrimSpace(v)
		if first == "" {
			first = v
		}
		if section == `[remote "origin"]` {
			origin = v
		}
	}
	if origin != "" {
		return origin
	}
	return first
}

var scpLike = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+)$`)

// NormalizeRemote maps the many spellings of a remote to host/path:
//
//	git@github.com:Org/Repo.git        → github.com/org/repo
//	https://user@github.com/Org/Repo   → github.com/org/repo
//	ssh://git@host:2222/org/repo.git   → host/org/repo
func NormalizeRemote(url string) string {
	u := strings.TrimSpace(url)
	if u == "" {
		return ""
	}
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
		if at := strings.LastIndex(strings.SplitN(u, "/", 2)[0], "@"); at >= 0 {
			u = u[at+1:]
		}
		host, path, _ := strings.Cut(u, "/")
		host, _, _ = strings.Cut(host, ":") // drop port
		u = host + "/" + path
	} else if m := scpLike.FindStringSubmatch(u); m != nil && !strings.HasPrefix(u, "/") {
		u = m[1] + "/" + m[2]
	} else {
		// Local filesystem remote: keep it, it is still a stable identity.
		return "file:" + filepath.ToSlash(filepath.Clean(u))
	}
	u = strings.TrimSuffix(strings.TrimRight(u, "/"), ".git")
	return strings.ToLower(u)
}
