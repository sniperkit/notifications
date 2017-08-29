// Package githubapi implements notifications.Service using GitHub API client.
package githubapi

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/github"
	"github.com/shurcooL/notifications"
	"github.com/shurcooL/users"
)

// NewService creates a GitHub-backed notifications.Service using given GitHub clients.
// At this time it infers the current user from the client (its authentication info), and cannot be used to serve multiple users.
//
// Caching can't be used for Activity.ListNotifications until GitHub API fixes the
// odd behavior of returning 304 even when some notifications get marked as read.
// Otherwise read notifications remain forever (until a new notification comes in).
// That's why we need clientNoCache.
func NewService(clientNoCache *github.Client, client *github.Client) notifications.Service {
	return service{
		clNoCache: clientNoCache,
		cl:        client,
	}
}

type service struct {
	clNoCache *github.Client // TODO: Start using cache whenever possible, remove this.
	cl        *github.Client
}

func (s service) List(ctx context.Context, opt notifications.ListOptions) (notifications.Notifications, error) {
	var ns []notifications.Notification

	ghOpt := &github.NotificationListOptions{
		All:         opt.All,
		ListOptions: github.ListOptions{PerPage: 100},
	}
	var ghNotifications []*github.Notification
	switch opt.Repo {
	case nil:
		for {
			ns, resp, err := s.clNoCache.Activity.ListNotifications(ctx, ghOpt)
			if err != nil {
				return nil, err
			}
			ghNotifications = append(ghNotifications, ns...)
			if resp.NextPage == 0 {
				break
			}
			ghOpt.Page = resp.NextPage
		}
	default:
		repo, err := ghRepoSpec(*opt.Repo)
		if err != nil {
			return nil, err
		}
		for {
			ns, resp, err := s.clNoCache.Activity.ListRepositoryNotifications(ctx, repo.Owner, repo.Repo, ghOpt)
			if err != nil {
				return nil, err
			}
			ghNotifications = append(ghNotifications, ns...)
			if resp.NextPage == 0 {
				break
			}
			ghOpt.Page = resp.NextPage
		}
	}
	for _, n := range ghNotifications {
		notification := notifications.Notification{
			AppID:     *n.Subject.Type,
			RepoSpec:  notifications.RepoSpec{URI: "github.com/" + *n.Repository.FullName},
			RepoURL:   "https://github.com/" + *n.Repository.FullName,
			Title:     *n.Subject.Title,
			UpdatedAt: *n.UpdatedAt,
			Read:      !*n.Unread,

			Participating: *n.Reason != "subscribed", // According to https://developer.github.com/v3/activity/notifications/#notification-reasons, "subscribed" reason means "you're watching the repository", and all other reasons imply participation.
		}

		// This makes a single API call. It's relatively slow/expensive
		// because it happens in the ghNotifications loop.
		if actor, err := s.getNotificationActor(ctx, *n.Subject); err == nil {
			notification.Actor = actor
		}

		switch *n.Subject.Type {
		case "Issue":
			rs, issueID, err := parseIssueSpec(*n.Subject.URL)
			if err != nil {
				return ns, err
			}
			notification.ThreadID = issueID
			switch state, err := s.getIssueState(ctx, *n.Subject.URL); {
			case err == nil && state == "open":
				notification.Icon = "issue-opened"
				notification.Color = notifications.RGB{R: 0x6c, G: 0xc6, B: 0x44} // Green.
			case err == nil && state == "closed":
				notification.Icon = "issue-closed"
				notification.Color = notifications.RGB{R: 0xbd, G: 0x2c, B: 0x00} // Red.
			default:
				notification.Icon = "issue-opened"
			}
			notification.HTMLURL, err = getIssueURL(rs, issueID, n.Subject.LatestCommentURL)
			if err != nil {
				return ns, err
			}
		case "PullRequest":
			rs, prID, err := parsePullRequestSpec(*n.Subject.URL)
			if err != nil {
				return ns, err
			}
			notification.ThreadID = prID
			notification.Icon = "git-pull-request"
			switch state, err := s.getPullRequestState(ctx, *n.Subject.URL); {
			case err == nil && state == "open":
				notification.Color = notifications.RGB{R: 0x6c, G: 0xc6, B: 0x44} // Green.
			case err == nil && state == "closed":
				notification.Color = notifications.RGB{R: 0xbd, G: 0x2c, B: 0x00} // Red.
			case err == nil && state == "merged":
				notification.Color = notifications.RGB{R: 0x6e, G: 0x54, B: 0x94} // Purple.
			default:
				log.Println("githubapi.service.List: getPullRequestState:", err)
			}
			notification.HTMLURL, err = getPullRequestURL(rs, prID, n.Subject.LatestCommentURL)
			if err != nil {
				return ns, err
			}
		case "Commit":
			id, err := strconv.ParseUint(*n.ID, 10, 64)
			if err != nil {
				return ns, fmt.Errorf("notifications/githubapi: failed to parse Commit notification ID %q to uint64: %v", *n.ID, err)
			}
			notification.ThreadID = id
			notification.Icon = "git-commit"
			notification.Color = notifications.RGB{R: 0x76, G: 0x76, B: 0x76} // Gray.
			notification.HTMLURL, err = getCommitURL(*n.Subject)
			if err != nil {
				return ns, err
			}
		case "Release":
			id, err := strconv.ParseUint(*n.ID, 10, 64)
			if err != nil {
				return ns, fmt.Errorf("notifications/githubapi: failed to parse Release notification ID %q to uint64: %v", *n.ID, err)
			}
			notification.ThreadID = id
			notification.Icon = "tag"
			notification.Color = notifications.RGB{R: 0x76, G: 0x76, B: 0x76} // Gray.
			notification.HTMLURL, err = s.getReleaseURL(ctx, *n.Subject.URL)
			if err != nil {
				return ns, err
			}
		case "RepositoryInvitation":
			id, err := strconv.ParseUint(*n.ID, 10, 64)
			if err != nil {
				return ns, fmt.Errorf("notifications/githubapi: failed to parse RepositoryInvitation notification ID %q to uint64: %v", *n.ID, err)
			}
			notification.ThreadID = id
			notification.Icon = "mail"
			notification.Color = notifications.RGB{R: 0x76, G: 0x76, B: 0x76} // Gray.
			notification.HTMLURL = "https://github.com/" + *n.Repository.FullName + "/invitations"
		default:
			log.Printf("unsupported *n.Subject.Type: %q\n", *n.Subject.Type)
		}

		ns = append(ns, notification)
	}

	return ns, nil
}

func (s service) Count(ctx context.Context, opt interface{}) (uint64, error) {
	ghOpt := &github.NotificationListOptions{ListOptions: github.ListOptions{PerPage: 1}}
	ghNotifications, resp, err := s.clNoCache.Activity.ListNotifications(ctx, ghOpt)
	if err != nil {
		return 0, err
	}
	if resp.LastPage != 0 {
		return uint64(resp.LastPage), nil
	} else {
		return uint64(len(ghNotifications)), nil
	}
}

func (s service) MarkRead(ctx context.Context, appID string, rs notifications.RepoSpec, threadID uint64) error {
	if appID == "Commit" || appID == "Release" || appID == "RepositoryInvitation" {
		_, err := s.cl.Activity.MarkThreadRead(ctx, strconv.FormatUint(threadID, 10))
		if err != nil {
			return fmt.Errorf("MarkRead: failed to MarkThreadRead: %v", err)
		}
		return nil
	}

	// Note: If we can always parse the notification ID (a numeric string like "230400425")
	//       from GitHub into a uint64 reliably, then we can skip the whole list repo notifications
	//       and match stuff dance, and just do Activity.MarkThreadRead(ctx, threadID) directly...
	// Update: Not quite. We need to return actual issue IDs as ThreadIDs in List, so that
	//         issuesapp.augmentUnread works correctly. But maybe if we can store it in another
	//         field...

	repo, err := ghRepoSpec(rs)
	if err != nil {
		return err
	}
	// It's okay to use with-cache client here, because we don't mind seeing read notifications
	// for the purposes of MarkRead. They'll be skipped if the notification ID doesn't match.
	ns, _, err := s.cl.Activity.ListRepositoryNotifications(ctx, repo.Owner, repo.Repo, nil)
	if err != nil {
		return fmt.Errorf("failed to ListRepositoryNotifications: %v", err)
	}
	for _, n := range ns {
		if *n.Subject.Type != appID {
			continue
		}

		var id uint64
		switch *n.Subject.Type {
		case "Issue":
			_, id, err = parseIssueSpec(*n.Subject.URL)
			if err != nil {
				return fmt.Errorf("failed to parseIssueSpec: %v", err)
			}
		case "PullRequest":
			_, id, err = parsePullRequestSpec(*n.Subject.URL)
			if err != nil {
				return fmt.Errorf("failed to parsePullRequestSpec: %v", err)
			}
		case "Commit", "Release", "RepositoryInvitation":
			// These thread types are already handled on top, so skip them.
			continue
		default:
			return fmt.Errorf("MarkRead: unsupported *n.Subject.Type: %v", *n.Subject.Type)
		}
		if id != threadID {
			continue
		}

		_, err = s.cl.Activity.MarkThreadRead(ctx, *n.ID)
		if err != nil {
			return fmt.Errorf("MarkRead: failed to MarkThreadRead: %v", err)
		}
		return nil
	}
	return fmt.Errorf("MarkRead: did not find notification %s/%d within %d notifications of repo %s:\n%v", appID, threadID, len(ns), rs.URI, notificationsString(ns))
}

// notificationsString returns a string representation of notifications ns.
func notificationsString(ns []*github.Notification) string {
	var ss []string
	for _, n := range ns {
		ss = append(ss, fmt.Sprintf("%v\t%v/%v", strings.TrimPrefix(*n.Subject.URL, "https://api.github.com/"), *n.Subject.Type, *n.ID))
	}
	return strings.Join(ss, "\n")
}

func (s service) MarkAllRead(ctx context.Context, rs notifications.RepoSpec) error {
	repo, err := ghRepoSpec(rs)
	if err != nil {
		return err
	}
	_, err = s.cl.Activity.MarkRepositoryNotificationsRead(ctx, repo.Owner, repo.Repo, time.Now())
	if err != nil {
		return fmt.Errorf("failed to MarkRepositoryNotificationsRead: %v", err)
	}
	return nil
}

func (s service) Notify(ctx context.Context, appID string, repo notifications.RepoSpec, threadID uint64, op notifications.NotificationRequest) error {
	// Nothing to do. GitHub takes care of this on their end, even when creating comments/issues via API.
	return nil
}

func (s service) Subscribe(ctx context.Context, appID string, repo notifications.RepoSpec, threadID uint64, subscribers []users.UserSpec) error {
	// Nothing to do. GitHub takes care of this on their end, even when creating comments/issues via API.
	return nil
}

// getNotificationActor tries to follow the LatestCommentURL, if not-nil,
// to fetch an object that contains a User or Author, who is taken to be
// the actor that triggered the notification.
func (s service) getNotificationActor(ctx context.Context, subject github.NotificationSubject) (users.User, error) {
	var apiURL string
	if subject.LatestCommentURL != nil {
		apiURL = *subject.LatestCommentURL
	} else if subject.URL != nil {
		// URL is used as fallback, if LatestCommentURL isn't present. It can happen for inline comments on PRs, etc.
		apiURL = *subject.URL
	} else {
		// This can happen if the event comes from a private repository
		// and we don't have any API URL values for it.
		return users.User{}, fmt.Errorf("subject API URLs not present")
	}
	req, err := s.cl.NewRequest("GET", apiURL, nil)
	if err != nil {
		return users.User{}, err
	}
	n := new(struct {
		User   *github.User
		Author *github.User
	})
	_, err = s.cl.Do(ctx, req, n)
	if err != nil {
		return users.User{}, err
	}
	if n.User != nil {
		return ghUser(n.User), nil
	} else if n.Author != nil {
		// Author is used as fallback, if User isn't present. It can happen on releases, etc.
		return ghUser(n.Author), nil
	} else {
		return users.User{}, fmt.Errorf("both User and Author are nil for %q: %v", apiURL, n)
	}
}

func (s service) getIssueState(ctx context.Context, issueAPIURL string) (string, error) {
	req, err := s.cl.NewRequest("GET", issueAPIURL, nil)
	if err != nil {
		return "", err
	}
	issue := new(github.Issue)
	_, err = s.cl.Do(ctx, req, issue)
	if err != nil {
		return "", err
	}
	if issue.State == nil {
		return "", fmt.Errorf("for some reason issue.State is nil for %q: %v", issueAPIURL, issue)
	}
	return *issue.State, nil
}

func (s service) getPullRequestState(ctx context.Context, prAPIURL string) (string, error) {
	req, err := s.cl.NewRequest("GET", prAPIURL, nil)
	if err != nil {
		return "", err
	}
	pr := new(github.PullRequest)
	_, err = s.cl.Do(ctx, req, pr)
	if err != nil {
		return "", err
	}
	if pr.State == nil || pr.Merged == nil {
		return "", fmt.Errorf("for some reason pr.State or pr.Merged is nil for %q: %v", prAPIURL, pr)
	}
	switch {
	default:
		return *pr.State, nil
	case *pr.Merged:
		return "merged", nil
	}
}

func getIssueURL(rs notifications.RepoSpec, issueID uint64, commentURL *string) (string, error) {
	var fragment string
	if _, commentID, err := parseCommentSpec(commentURL); err == nil {
		fragment = fmt.Sprintf("#comment-%d", commentID)
	}
	return fmt.Sprintf("https://github.com/%s/issues/%d%s", rs.URI, issueID, fragment), nil
}

func getPullRequestURL(rs notifications.RepoSpec, prID uint64, commentURL *string) (string, error) {
	var fragment string
	if _, commentID, err := parseCommentSpec(commentURL); err == nil {
		fragment = fmt.Sprintf("#comment-%d", commentID)
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d%s", rs.URI, prID, fragment), nil
}

func getCommitURL(subject github.NotificationSubject) (string, error) {
	rs, commit, err := parseSpec(*subject.URL, "commits")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("https://github.com/%s/commit/%s", rs.URI, commit), nil
}

func (s service) getReleaseURL(ctx context.Context, releaseAPIURL string) (string, error) {
	req, err := s.cl.NewRequest("GET", releaseAPIURL, nil)
	if err != nil {
		return "", err
	}
	rr := new(github.RepositoryRelease)
	_, err = s.cl.Do(ctx, req, rr)
	if err != nil {
		return "", err
	}
	return *rr.HTMLURL, nil
}

func parseIssueSpec(issueAPIURL string) (_ notifications.RepoSpec, issueID uint64, _ error) {
	rs, id, err := parseSpec(issueAPIURL, "issues")
	if err != nil {
		return notifications.RepoSpec{}, 0, err
	}
	issueID, err = strconv.ParseUint(id, 10, 64)
	if err != nil {
		return notifications.RepoSpec{}, 0, err
	}
	return rs, issueID, nil
}

func parsePullRequestSpec(prAPIURL string) (_ notifications.RepoSpec, prID uint64, _ error) {
	rs, id, err := parseSpec(prAPIURL, "pulls")
	if err != nil {
		return notifications.RepoSpec{}, 0, err
	}
	prID, err = strconv.ParseUint(id, 10, 64)
	if err != nil {
		return notifications.RepoSpec{}, 0, err
	}
	return rs, prID, nil
}

func parseSpec(apiURL, specType string) (_ notifications.RepoSpec, id string, _ error) {
	u, err := url.Parse(apiURL)
	if err != nil {
		return notifications.RepoSpec{}, "", err
	}
	e := strings.Split(u.Path, "/")
	if len(e) < 5 {
		return notifications.RepoSpec{}, "", fmt.Errorf("unexpected path (too few elements): %q", u.Path)
	}
	if got, want := e[len(e)-2], specType; got != want {
		return notifications.RepoSpec{}, "", fmt.Errorf("unexpected path element %q, expecting %q", got, want)
	}
	return notifications.RepoSpec{URI: e[len(e)-4] + "/" + e[len(e)-3]}, e[len(e)-1], nil
}

func parseCommentSpec(commentURL *string) (notifications.RepoSpec, int, error) {
	if commentURL == nil {
		// This can happen if the event comes from a private repository
		// and we don't have a LatestCommentURL value for it.
		return notifications.RepoSpec{}, 0, fmt.Errorf("comment URL not present")
	}
	u, err := url.Parse(*commentURL)
	if err != nil {
		return notifications.RepoSpec{}, 0, err
	}
	e := strings.Split(u.Path, "/")
	if len(e) < 6 {
		return notifications.RepoSpec{}, 0, fmt.Errorf("unexpected path (too few elements): %q", u.Path)
	}
	if got, want := e[len(e)-2], "comments"; got != want {
		return notifications.RepoSpec{}, 0, fmt.Errorf("unexpected path element %q, expecting %q", got, want)
	}
	if got, want := e[len(e)-3], "issues"; got != want {
		return notifications.RepoSpec{}, 0, fmt.Errorf("unexpected path element %q, expecting %q", got, want)
	}
	id, err := strconv.Atoi(e[len(e)-1])
	if err != nil {
		return notifications.RepoSpec{}, 0, err
	}
	return notifications.RepoSpec{URI: e[len(e)-5] + "/" + e[len(e)-4]}, id, nil
}

type repoSpec struct {
	Owner string
	Repo  string
}

func ghRepoSpec(repo notifications.RepoSpec) (repoSpec, error) {
	// TODO, THINK: Include "github.com/" prefix or not?
	//              So far I'm leaning towards "yes", because it's more definitive and matches
	//              local uris that also include host. This way, the host can be checked as part of
	//              request, rather than kept implicit.
	ghOwnerRepo := strings.Split(repo.URI, "/")
	if len(ghOwnerRepo) != 3 || ghOwnerRepo[0] != "github.com" || ghOwnerRepo[1] == "" || ghOwnerRepo[2] == "" {
		return repoSpec{}, fmt.Errorf(`RepoSpec is not of form "github.com/owner/repo": %q`, repo.URI)
	}
	return repoSpec{
		Owner: ghOwnerRepo[1],
		Repo:  ghOwnerRepo[2],
	}, nil
}

func ghUser(user *github.User) users.User {
	return users.User{
		UserSpec: users.UserSpec{
			ID:     uint64(*user.ID),
			Domain: "github.com",
		},
		Login:     *user.Login,
		AvatarURL: avatarURLSize(*user.AvatarURL, 36),
		HTMLURL:   *user.HTMLURL,
	}
}

// avatarURLSize takes avatarURL and sets its "s" query parameter to size.
func avatarURLSize(avatarURL string, size int) string {
	u, err := url.Parse(avatarURL)
	if err != nil {
		return avatarURL
	}
	q := u.Query()
	q.Set("s", fmt.Sprint(size))
	u.RawQuery = q.Encode()
	return u.String()
}
