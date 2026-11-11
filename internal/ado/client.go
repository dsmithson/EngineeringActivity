// Package ado adapts Microsoft's Azure DevOps Go SDK to the facts the collector needs.
// Domain code depends on the Source interface, so tests run without Azure DevOps.
package ado

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dsmithson/EngineeringActivity/internal/facts"
	"github.com/google/uuid"
	"github.com/microsoft/azure-devops-go-api/azuredevops/v7"
	"github.com/microsoft/azure-devops-go-api/azuredevops/v7/git"
	"github.com/microsoft/azure-devops-go-api/azuredevops/v7/workitemtracking"
)

// Source is everything the collector reads from Azure DevOps.
type Source interface {
	Repository(ctx context.Context, name string) (facts.Repository, error)
	WorkItemTypeStates(ctx context.Context, workItemType string) ([]string, error)
	// ChangedWorkItemIDs lists work items of the given types changed at or after since.
	ChangedWorkItemIDs(ctx context.Context, types []string, since time.Time) ([]int, error)
	WorkItemRevisions(ctx context.Context, id int) ([]facts.Revision, error)
	// PullRequests lists PRs of any status. With a zero query time range every PR in the
	// repository is listed (backfill); otherwise PRs created or closed within the range.
	PullRequests(ctx context.Context, repositoryID string, q PRQuery, page func([]facts.PullRequest) error) error
}

type PRQuery struct {
	TimeRangeType string // "created" or "closed"; empty lists all
	Min, Max      time.Time
}

type Client struct {
	Organization string
	Project      string
	git          *git.ClientImpl
	wit          *workitemtracking.ClientImpl
}

// New connects SDK clients for organization (e.g. https://dev.azure.com/org) through the
// supplied HTTP client, which carries authentication, retries, and accounting.
func New(orgURL, organization, project string, httpClient *http.Client) *Client {
	conn := azuredevops.NewAnonymousConnection(orgURL)
	conn.UserAgent = "engineering-activity-collector"
	base := azuredevops.NewClientWithOptions(conn, strings.TrimRight(orgURL, "/"), azuredevops.WithHTTPClient(httpClient))
	return &Client{
		Organization: organization,
		Project:      project,
		git:          &git.ClientImpl{Client: *base},
		wit:          &workitemtracking.ClientImpl{Client: *base},
	}
}

func (c *Client) Repository(ctx context.Context, name string) (facts.Repository, error) {
	r, err := c.git.GetRepository(ctx, git.GetRepositoryArgs{RepositoryId: &name, Project: &c.Project})
	if err != nil {
		return facts.Repository{}, err
	}
	if r.Id == nil || r.Name == nil {
		return facts.Repository{}, errors.New("repository response missing id")
	}
	repo := facts.Repository{ID: r.Id.String(), Organization: c.Organization, Project: c.Project, Name: *r.Name}
	if r.WebUrl != nil {
		repo.WebURL = *r.WebUrl
	}
	return repo, nil
}

func (c *Client) WorkItemTypeStates(ctx context.Context, workItemType string) ([]string, error) {
	states, err := c.wit.GetWorkItemTypeStates(ctx, workitemtracking.GetWorkItemTypeStatesArgs{Project: &c.Project, Type: &workItemType})
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, s := range *states {
		if s.Name != nil {
			out = append(out, *s.Name)
		}
	}
	return out, nil
}

// WIQL returns at most 20,000 results; ranges at the limit are bisected by changed date.
const wiqlLimit = 19999

func wiqlString(s string) string { return strings.ReplaceAll(s, "'", "''") }

func (c *Client) ChangedWorkItemIDs(ctx context.Context, types []string, since time.Time) ([]int, error) {
	if len(types) == 0 {
		return nil, errors.New("no work item types configured")
	}
	quoted := make([]string, len(types))
	for i, t := range types {
		quoted[i] = "'" + wiqlString(t) + "'"
	}
	ids := map[int]bool{}
	var query func(from, to time.Time) error
	query = func(from, to time.Time) error {
		wiql := fmt.Sprintf("SELECT [System.Id] FROM WorkItems WHERE [System.TeamProject] = '%s' AND [System.WorkItemType] IN (%s) AND [System.ChangedDate] >= '%s' AND [System.ChangedDate] < '%s' ORDER BY [System.Id]",
			wiqlString(c.Project), strings.Join(quoted, ", "), from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
		precise, top := true, wiqlLimit+1
		res, err := c.wit.QueryByWiql(ctx, workitemtracking.QueryByWiqlArgs{Wiql: &workitemtracking.Wiql{Query: &wiql}, Project: &c.Project, TimePrecision: &precise, Top: &top})
		if err != nil {
			return err
		}
		if res.WorkItems == nil {
			return nil
		}
		if len(*res.WorkItems) > wiqlLimit {
			if to.Sub(from) <= time.Second {
				return errors.New("work item query exceeds the WIQL limit within one second")
			}
			mid := from.Add(to.Sub(from) / 2).Truncate(time.Second)
			if err := query(from, mid); err != nil {
				return err
			}
			return query(mid, to)
		}
		for _, w := range *res.WorkItems {
			if w.Id != nil {
				ids[*w.Id] = true
			}
		}
		return nil
	}
	// Upper bound in the future so items changed while this run is in progress are included.
	if err := query(since, time.Now().UTC().Add(24*time.Hour).Truncate(time.Second)); err != nil {
		return nil, err
	}
	out := make([]int, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Ints(out)
	return out, nil
}

func (c *Client) WorkItemRevisions(ctx context.Context, id int) ([]facts.Revision, error) {
	const pageSize = 200
	out := []facts.Revision{}
	for skip := 0; ; skip += pageSize {
		top, s := pageSize, skip
		page, err := c.wit.GetRevisions(ctx, workitemtracking.GetRevisionsArgs{Id: &id, Project: &c.Project, Top: &top, Skip: &s})
		if err != nil {
			return nil, err
		}
		for _, w := range *page {
			if w.Rev == nil || w.Fields == nil {
				return nil, fmt.Errorf("work item %d: revision without fields", id)
			}
			out = append(out, facts.Revision{Rev: *w.Rev, Fields: *w.Fields})
		}
		if len(*page) < pageSize {
			return out, nil
		}
	}
}

// pullRequestsLocation is the SDK's location id for "Get Pull Requests".
var pullRequestsLocation = uuid.MustParse("9946fd70-0d40-406e-b686-b4744cbbcc37")

// PullRequests uses the SDK transport and response types, adding the time-range criteria
// (searchCriteria.minTime/maxTime/queryTimeRangeType, REST 7.1) that the v7.1.0 SDK's
// GitPullRequestSearchCriteria does not expose. Documented SDK coverage gap.
func (c *Client) PullRequests(ctx context.Context, repositoryID string, q PRQuery, page func([]facts.PullRequest) error) error {
	const pageSize = 100
	for skip := 0; ; skip += pageSize {
		params := url.Values{}
		params.Set("searchCriteria.status", "all")
		if q.TimeRangeType != "" {
			params.Set("searchCriteria.queryTimeRangeType", q.TimeRangeType)
			params.Set("searchCriteria.minTime", q.Min.UTC().Format(time.RFC3339))
			params.Set("searchCriteria.maxTime", q.Max.UTC().Format(time.RFC3339))
		}
		params.Set("$top", strconv.Itoa(pageSize))
		params.Set("$skip", strconv.Itoa(skip))
		resp, err := c.git.Client.Send(ctx, http.MethodGet, pullRequestsLocation, "7.1", map[string]string{"project": c.Project, "repositoryId": repositoryID}, params, nil, "", "application/json", nil)
		if err != nil {
			return err
		}
		var prs []git.GitPullRequest
		if err := c.git.Client.UnmarshalCollectionBody(resp, &prs); err != nil {
			return err
		}
		converted := make([]facts.PullRequest, 0, len(prs))
		for _, p := range prs {
			f, err := ConvertPullRequest(repositoryID, p)
			if err != nil {
				return err
			}
			converted = append(converted, f)
		}
		if err := page(converted); err != nil {
			return err
		}
		if len(prs) < pageSize {
			return nil
		}
	}
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func timeOf(t *azuredevops.Time) *time.Time {
	if t == nil || t.Time.IsZero() || t.Time.Year() < 2000 {
		return nil
	}
	v := t.Time.UTC()
	return &v
}

// ConvertPullRequest maps an SDK pull request to facts. Completion snapshot commits are
// lastMergeSourceCommit/lastMergeTargetCommit as recorded by the service.
func ConvertPullRequest(repositoryID string, p git.GitPullRequest) (facts.PullRequest, error) {
	if p.PullRequestId == nil || p.CreationDate == nil {
		return facts.PullRequest{}, errors.New("pull request response missing id or creation date")
	}
	pr := facts.PullRequest{
		RepositoryID: strings.ToLower(repositoryID),
		ID:           *p.PullRequestId,
		Title:        str(p.Title),
		SourceRef:    str(p.SourceRefName),
		TargetRef:    str(p.TargetRefName),
		CreatedAt:    p.CreationDate.Time.UTC(),
		ClosedAt:     timeOf(p.ClosedDate),
		Creator:      facts.Unknown,
	}
	if p.Status != nil {
		pr.Status = string(*p.Status)
	}
	if p.IsDraft != nil {
		pr.IsDraft = *p.IsDraft
	}
	if p.CreatedBy != nil && p.CreatedBy.Id != nil {
		pr.Creator = facts.NewADOIdentity(*p.CreatedBy.Id, str(p.CreatedBy.DisplayName), str(p.CreatedBy.UniqueName))
	}
	if pr.Status == "completed" {
		pr.CompletedAt = pr.ClosedAt
	}
	if p.LastMergeSourceCommit != nil {
		pr.LastMergeSourceSHA = strings.ToLower(str(p.LastMergeSourceCommit.CommitId))
	}
	if p.LastMergeTargetCommit != nil {
		pr.LastMergeTargetSHA = strings.ToLower(str(p.LastMergeTargetCommit.CommitId))
	}
	return pr, nil
}
