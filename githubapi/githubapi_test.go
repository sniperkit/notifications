package githubapi

import (
	"testing"

	"github.com/google/go-github/github"
)

func TestGetCommitURL(t *testing.T) {
	url := "https://api.github.com/repos/owner/name/commits/63552f503fd0adeaf7401c40f7f24412e2e6aa6b"
	n := github.NotificationSubject{
		URL: &url,
	}
	got, err := getCommitURL(n)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://github.com/owner/name/commit/63552f503fd0adeaf7401c40f7f24412e2e6aa6b"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestAvatarURLSize(t *testing.T) {
	got := avatarURLSize("https://avatars.githubusercontent.com/u/12345?v=3", 36)
	want := "https://avatars.githubusercontent.com/u/12345?s=36&v=3"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestNotificationsString(t *testing.T) {
	ns := []*github.Notification{
		{
			Subject: &github.NotificationSubject{
				URL:  github.String("https://api.github.com/repos/neugram/ng/issues/10"),
				Type: github.String("Issue"),
			},
			ID: github.String("271670023"),
		},
		{
			Subject: &github.NotificationSubject{
				URL:  github.String("https://api.github.com/repos/neugram/ng/pulls/22"),
				Type: github.String("PullRequest"),
			},
			ID: github.String("271863360"),
		},
		{
			Subject: &github.NotificationSubject{
				URL:  github.String("https://api.github.com/repos/neugram/ng/pulls/21"),
				Type: github.String("PullRequest"),
			},
			ID: github.String("271857043"),
		},
	}
	got := notificationsString(ns)
	want := `	repos/neugram/ng/issues/10
	repos/neugram/ng/pulls/22
	repos/neugram/ng/pulls/21`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
