package target

import (
	"testing"

	"github.com/MarvAmBass/gitbackup/internal/provider"
)

func TestParse(t *testing.T) {
	type want struct {
		p          provider.Platform
		host, kind string
		path       string
	}
	for _, tc := range []struct {
		in   string
		hint provider.Platform
		w    want
	}{
		{"example-user", "", want{provider.GitHub, "github.com", Account, "example-user"}},
		{"https://github.com/octo-org", "", want{provider.GitHub, "github.com", Account, "octo-org"}},
		{"github.com/pi-hole/pi-hole", "", want{provider.GitHub, "github.com", Repo, "pi-hole/pi-hole"}},
		{"pi-hole/pi-hole", "", want{provider.GitHub, "github.com", Repo, "pi-hole/pi-hole"}},
		{"https://github.com/a/b/tree/main/x", "", want{provider.GitHub, "github.com", Repo, "a/b"}},
		{"git@github.com:a/b.git", "", want{provider.GitHub, "github.com", Repo, "a/b"}},
		{"https://gitlab.com/gitlab-org", "", want{provider.GitLab, "gitlab.com", Account, "gitlab-org"}},
		{"https://gitlab.com/gitlab-org/gitlab-runner/-/tree/main", "", want{provider.GitLab, "gitlab.com", Auto, "gitlab-org/gitlab-runner"}},
		{"gitlab-org/security", provider.GitLab, want{provider.GitLab, "gitlab.com", Auto, "gitlab-org/security"}},
		{"https://codeberg.org/forgejo/forgejo.git", "", want{provider.Gitea, "codeberg.org", Repo, "forgejo/forgejo"}},
		{"https://codeberg.org/forgejo/forgejo/src/branch/forgejo", "", want{provider.Gitea, "codeberg.org", Repo, "forgejo/forgejo"}},
		{"forgejo", provider.Gitea, want{provider.Gitea, "codeberg.org", Account, "forgejo"}},
		{"~sircmpwn", "", want{provider.Sourcehut, "git.sr.ht", Account, "~sircmpwn"}},
		{"https://git.sr.ht/~sircmpwn/scdoc", "", want{provider.Sourcehut, "git.sr.ht", Repo, "~sircmpwn/scdoc"}},
		{"https://git.company.lan/team/tool", provider.Gitea, want{provider.Gitea, "git.company.lan", Repo, "team/tool"}},
		{"git://git.codemadness.org/stagit", provider.Git, want{provider.Git, "git.codemadness.org", Repo, "git://git.codemadness.org/stagit"}},
		{"file:///srv/git/tools.git", "", want{provider.Git, "local", Repo, "file:///srv/git/tools.git"}},
		{"https://git.company.lan/team/tool", "", want{"", "git.company.lan", "", "/team/tool"}}, // unknown → detect
	} {
		got, err := Parse(tc.in, tc.hint)
		if err != nil {
			t.Errorf("%s: %v", tc.in, err)
			continue
		}
		if g := (want{got.Platform, got.Host, got.Kind, got.Path}); g != tc.w {
			t.Errorf("%s (%s):\n got  %+v\n want %+v", tc.in, tc.hint, g, tc.w)
		}
	}
	for _, bad := range []string{"", "https://github.com/", "x", "https://github.com/a/../b"} {
		hint := provider.Platform("")
		if bad == "x" {
			hint = provider.Git
		}
		if _, err := Parse(bad, hint); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
	if h, o, n := PlainRepo("git@gitlab.company.lan:group/sub/proj.git"); h != "gitlab.company.lan" || o != "group/sub" || n != "proj" {
		t.Errorf("plain repo %s %s %s", h, o, n)
	}
	for got, want := range map[string]string{
		Slot("repo", "git.sr.ht", "~sircmpwn", "scdoc"):              "git.sr.ht/~sircmpwn/repos/scdoc",
		Slot("repo", "gitlab.com", "group/sub", "proj"):              "gitlab.com/group/sub/repos/proj",
		Slot("gist", "github.com", "bob", "abc"):                     "github.com/bob/gists/abc",
		Slot("repo", "github.com", "../../etc", "x"):                 "github.com/etc/repos/x",
		ForkSlot("github.com/pi-hole/repos/pi-hole", "example-user"): "github.com/pi-hole/repos/pi-hole/forks/example-user",
		StarLink("github.com", "example-user", "pi-hole", "FTL"):     "github.com/example-user/stars/pi-hole/FTL",
		GitDir("github.com/a/repos/b", RepoUnit, 0):                  "github.com/a/repos/b/repo.git",
		GitDir("github.com/a/repos/b", WikiUnit, 2):                  "github.com/a/repos/b/wiki.v2.git",
		HTMLDir("github.com/a/repos/b", 1):                           "github.com/a/repos/b/html.v1",
	} {
		if got != want {
			t.Errorf("layout: got %s, want %s", got, want)
		}
	}
}
