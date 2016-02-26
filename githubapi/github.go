// Package githubapi implements notifications.Service using GitHub API client.
package githubapi

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/go-github/github"
	"golang.org/x/net/context"
	"src.sourcegraph.com/apps/notifications/notifications"
)

// NewService creates a GitHub-backed notifications.Service using given GitHub client.
// At this time it infers the current user from the client (its authentication info), and cannot be used to serve multiple users.
func NewService(client *github.Client) notifications.Service {
	if client == nil {
		client = github.NewClient(nil)
	}

	s := service{
		cl: client,
	}

	if user, _, err := client.Users.Get(""); err == nil {
		u := ghUser(user)
		s.currentUser = &u
		s.currentUserErr = nil
	} else if ghErr, ok := err.(*github.ErrorResponse); ok && ghErr.Response.StatusCode == http.StatusUnauthorized {
		// There's no authenticated user.
		s.currentUser = nil
		s.currentUserErr = nil
	} else {
		s.currentUser = nil
		s.currentUserErr = err
	}

	return s
}

type service struct {
	cl *github.Client

	currentUser    *notifications.User
	currentUserErr error
}

func (s service) CurrentUser(_ context.Context) (*notifications.User, error) {
	return s.currentUser, s.currentUserErr
}

type repoSpec struct {
	Owner string
	Repo  string
}

func (s service) List(ctx context.Context, opt interface{}) (notifications.Notifications, error) {
	var ns []notifications.Notification

	ghNotifications, _, err := s.cl.Activity.ListNotifications(nil)
	if err != nil {
		return nil, err
	}
	for _, n := range ghNotifications {
		notification := notifications.Notification{
			RepoSpec:  notifications.RepoSpec{URI: *n.Repository.FullName},
			RepoURL:   template.URL("https://github.com/" + *n.Repository.FullName),
			Title:     *n.Subject.Title,
			UpdatedAt: *n.UpdatedAt,
		}

		switch *n.Subject.Type {
		case "Issue":
			state, err := s.getIssueState(*n.Subject.URL)
			if err != nil {
				return ns, err
			}
			switch state {
			case "open":
				notification.Icon = "issue-opened"
			case "closed":
				notification.Icon = "issue-closed"
			}

			notification.HTMLURL, err = s.getIssueURL(*n.Subject)
			if err != nil {
				return ns, err
			}
		}

		ns = append(ns, notification)
	}

	return ns, nil
}

func (s service) Count(ctx context.Context, opt interface{}) (uint64, error) {
	ghNotifications, _, err := s.cl.Activity.ListNotifications(nil)
	return uint64(len(ghNotifications)), err
}

func (s service) MarkRead(ctx context.Context, appID string, rs notifications.RepoSpec, threadID uint64) error {
	repo := ghRepoSpec(rs)
	ns, _, err := s.cl.Activity.ListRepositoryNotifications(repo.Owner, repo.Repo, nil)
	if err != nil {
		return fmt.Errorf("failed to ListRepositoryNotifications: %v", err)
	}
	for _, n := range ns {
		if *n.Subject.Type != appID {
			continue
		}
		_, issueID, err := parseIssueSpec(*n.Subject.URL)
		if err != nil {
			return fmt.Errorf("failed to parseIssueSpec: %v", err)
		}
		if uint64(issueID) != threadID {
			continue
		}

		_, err = s.cl.Activity.MarkThreadRead(*n.ID)
		if err != nil {
			return fmt.Errorf("failed to MarkThreadRead: %v", err)
		}
		break
	}
	return nil
}

func (s service) Notify(ctx context.Context, appID string, repo notifications.RepoSpec, threadID uint64, op notifications.Notification) error {
	// Nothing to do. GitHub takes care of this on their end.
	// TODO: Confirm the above is true. Maybe this needs to be done after all when creating comments/issues via API.
	return nil
}

func (s service) Subscribe(ctx context.Context, appID string, repo notifications.RepoSpec, threadID uint64, subscribers []notifications.UserSpec) error {
	// Nothing to do. GitHub takes care of this on their end.
	// TODO: Confirm the above is true. Maybe this needs to be done after all when creating comments/issues via API.
	return nil
}

func (s service) getIssueState(issueAPIURL string) (string, error) {
	req, err := s.cl.NewRequest("GET", issueAPIURL, nil)
	if err != nil {
		return "", err
	}
	issue := new(github.Issue)
	_, err = s.cl.Do(req, issue)
	if err != nil {
		return "", err
	}
	if issue.State == nil {
		return "", fmt.Errorf("for some reason issue.State is nil for %q: %v", issueAPIURL, issue)
	}
	return *issue.State, nil
}

func (s service) getIssueURL(n github.NotificationSubject) (template.URL, error) {
	rs, issueID, err := parseIssueSpec(*n.URL)
	if err != nil {
		return "", err
	}
	var fragment string
	if _, commentID, err := parseIssueCommentSpec(*n.LatestCommentURL); err == nil {
		fragment = fmt.Sprintf("#comment-%d", commentID)
	}
	return template.URL(fmt.Sprintf("/github.com/%s/issues/%d%s", rs.URI, issueID, fragment)), nil
}

func parseIssueSpec(issueAPIURL string) (notifications.RepoSpec, int, error) {
	u, err := url.Parse(issueAPIURL)
	if err != nil {
		return notifications.RepoSpec{}, 0, err
	}
	e := strings.Split(u.Path, "/")
	if len(e) < 5 {
		return notifications.RepoSpec{}, 0, fmt.Errorf("unexpected path (too few elements): %q", u.Path)
	}
	if got, want := e[len(e)-2], "issues"; got != want {
		return notifications.RepoSpec{}, 0, fmt.Errorf(`unexpected path element %q, expecting %q`, got, want)
	}
	id, err := strconv.Atoi(e[len(e)-1])
	if err != nil {
		return notifications.RepoSpec{}, 0, err
	}
	return notifications.RepoSpec{URI: e[len(e)-4] + "/" + e[len(e)-3]}, id, nil
}

func parseIssueCommentSpec(issueAPIURL string) (notifications.RepoSpec, int, error) {
	u, err := url.Parse(issueAPIURL)
	if err != nil {
		return notifications.RepoSpec{}, 0, err
	}
	e := strings.Split(u.Path, "/")
	if len(e) < 6 {
		return notifications.RepoSpec{}, 0, fmt.Errorf("unexpected path (too few elements): %q", u.Path)
	}
	if got, want := e[len(e)-2], "comments"; got != want {
		return notifications.RepoSpec{}, 0, fmt.Errorf(`unexpected path element %q, expecting %q`, got, want)
	}
	if got, want := e[len(e)-3], "issues"; got != want {
		return notifications.RepoSpec{}, 0, fmt.Errorf(`unexpected path element %q, expecting %q`, got, want)
	}
	id, err := strconv.Atoi(e[len(e)-1])
	if err != nil {
		return notifications.RepoSpec{}, 0, err
	}
	return notifications.RepoSpec{URI: e[len(e)-5] + "/" + e[len(e)-4]}, id, nil
}

func ghRepoSpec(repo notifications.RepoSpec) repoSpec {
	ownerRepo := strings.Split(repo.URI, "/")
	if len(ownerRepo) != 2 {
		panic(fmt.Errorf(`RepoSpec is not of form "owner/repo": %v`, repo))
	}
	return repoSpec{
		Owner: ownerRepo[0],
		Repo:  ownerRepo[1],
	}
}

func ghUser(user *github.User) notifications.User {
	return notifications.User{
		UserSpec: notifications.UserSpec{
			ID:     uint64(*user.ID),
			Domain: "github.com",
		},
		Login:     *user.Login,
		AvatarURL: template.URL(*user.AvatarURL),
		HTMLURL:   template.URL(*user.HTMLURL),
	}
}