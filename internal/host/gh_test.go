package host

import "testing"

func TestParseRemote(t *testing.T) {
	cases := []struct {
		in, owner, repo string
		ok              bool
	}{
		{"https://github.com/o/r", "o", "r", true},
		{"https://github.com/o/r.git", "o", "r", true},
		{"git@github.com:o/r.git", "o", "r", true},
		{"git@github.com:o/r", "o", "r", true},
		{"ssh://git@github.com/o/r.git", "o", "r", true},
		{"https://x-access-token:TOK@github.com/o/r.git", "o", "r", true},
		{"https://gitlab.com/o/r.git", "", "", false}, // unsupported host
		{"https://github.com/only-one", "", "", false},
		{"git@github.com:-flag/r.git", "", "", false}, // flag-like owner
		{"not a url", "", "", false},
	}
	for _, c := range cases {
		_, owner, repo, err := ParseRemote(c.in)
		if c.ok {
			if err != nil || owner != c.owner || repo != c.repo {
				t.Errorf("ParseRemote(%q) = (%q,%q,%v), want (%q,%q,nil)", c.in, owner, repo, err, c.owner, c.repo)
			}
		} else if err == nil {
			t.Errorf("ParseRemote(%q): want error, got (%q,%q)", c.in, owner, repo)
		}
	}
}
