package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/tasklink"
)

type apiUser struct {
	DisplayName string `json:"displayName"`
	Name        string `json:"name"`
}

type apiComment struct {
	Author  apiUser `json:"author"`
	Created string  `json:"created"`
	Body    string  `json:"body"`
}

type apiCommentPage struct {
	Comments   []apiComment `json:"comments"`
	Total      int          `json:"total"`
	MaxResults int          `json:"maxResults"`
	StartAt    int          `json:"startAt"`
}

type named struct {
	Name string `json:"name"`
}

type apiIssue struct {
	Key    string                     `json:"key"`
	Fields map[string]json.RawMessage `json:"fields"`
}

type apiParent struct {
	Key    string `json:"key"`
	Fields struct {
		Summary string `json:"summary"`
	} `json:"fields"`
}

// GetIssue fetches an issue with its latest comments, parent and epic.
// Parent and epic failures are not fatal: the brief then has only the key
// (and the summary when Jira returned it with the issue).
func (c *Client) GetIssue(ctx context.Context, key string) (*Issue, error) {
	if !tasklink.ValidKey(key) {
		return nil, fmt.Errorf("jira: invalid issue key %q", key)
	}
	fields := []string{"summary", "description", "status", "issuetype", "parent"}
	if c.commentsLimit > 0 {
		fields = append(fields, "comment")
	}
	if c.epicField != "" {
		fields = append(fields, c.epicField)
	}
	raw, err := c.getIssueRaw(ctx, key, fields)
	if err != nil {
		return nil, err
	}
	is := &Issue{Key: raw.Key, URL: c.BrowseURL(raw.Key)}
	if is.Key == "" {
		is.Key, is.URL = key, c.BrowseURL(key)
	}
	is.Summary = oneLine(str(raw.Fields["summary"]))
	is.Description = truncate(str(raw.Fields["description"]))
	is.Status = nameOf(raw.Fields["status"])
	is.Type = nameOf(raw.Fields["issuetype"])

	if c.commentsLimit > 0 {
		is.Comments = c.comments(ctx, is.Key, raw.Fields["comment"])
	}

	var parentEpic string
	is.Parent, parentEpic = c.parent(ctx, raw.Fields["parent"])
	if c.epicField != "" {
		is.EpicKey = str(raw.Fields[c.epicField])
		if is.EpicKey == "" {
			// Sub-tasks inherit the epic of their parent.
			is.EpicKey = parentEpic
		}
		if !tasklink.ValidKey(is.EpicKey) {
			is.EpicKey = ""
		}
	}
	if is.EpicKey != "" && is.EpicKey != is.Key {
		is.Epic = c.epic(ctx, is.EpicKey)
	}
	return is, nil
}

// parent fetches the parent of a sub-task (the issue's parent field) and
// the parent's epic link. Nil when the issue has no parent.
func (c *Client) parent(ctx context.Context, field json.RawMessage) (*IssueBrief, string) {
	if len(field) == 0 || string(field) == "null" {
		return nil, ""
	}
	var ap apiParent
	if json.Unmarshal(field, &ap) != nil || !tasklink.ValidKey(ap.Key) {
		return nil, ""
	}
	brief, epic, err := c.brief(ctx, ap.Key, true)
	if err != nil {
		c.log.Warn("jira: fetch parent", "key", ap.Key, "err", err)
		brief = &IssueBrief{Key: ap.Key, URL: c.BrowseURL(ap.Key), Summary: oneLine(ap.Fields.Summary)}
	}
	return brief, epic
}

// epic fetches the epic brief (only the key and link when that fails).
func (c *Client) epic(ctx context.Context, key string) *IssueBrief {
	brief, _, err := c.brief(ctx, key, false)
	if err != nil {
		c.log.Warn("jira: fetch epic", "key", key, "err", err)
		brief = &IssueBrief{Key: key, URL: c.BrowseURL(key)}
	}
	return brief
}

// brief fetches summary + description (and the epic link when withEpic).
func (c *Client) brief(ctx context.Context, key string, withEpic bool) (*IssueBrief, string, error) {
	fields := []string{"summary", "description"}
	if withEpic && c.epicField != "" {
		fields = append(fields, c.epicField)
	}
	raw, err := c.getIssueRaw(ctx, key, fields)
	if err != nil {
		return nil, "", err
	}
	b := &IssueBrief{Key: key, URL: c.BrowseURL(key),
		Summary: oneLine(str(raw.Fields["summary"])), Description: truncate(str(raw.Fields["description"]))}
	epic := ""
	if withEpic && c.epicField != "" {
		epic = str(raw.Fields[c.epicField])
	}
	return b, epic, nil
}

func (c *Client) getIssueRaw(ctx context.Context, key string, fields []string) (*apiIssue, error) {
	q := url.Values{}
	q.Set("fields", strings.Join(fields, ","))
	var raw apiIssue
	if err := c.getJSON(ctx, c.endpoint(q, "rest", "api", "2", "issue", key), &raw); err != nil {
		return nil, err
	}
	return &raw, nil
}

// comments returns the latest commentsLimit comments, oldest first. The
// issue's comment field may hold only the first page; then the tail is
// fetched from the comment endpoint.
func (c *Client) comments(ctx context.Context, key string, field json.RawMessage) []Comment {
	var page apiCommentPage
	if len(field) == 0 || json.Unmarshal(field, &page) != nil {
		return nil
	}
	list := page.Comments
	if page.Total > page.StartAt+len(list) {
		q := url.Values{}
		q.Set("startAt", strconv.Itoa(max(page.Total-c.commentsLimit, 0)))
		q.Set("maxResults", strconv.Itoa(c.commentsLimit))
		var tail apiCommentPage
		if err := c.getJSON(ctx, c.endpoint(q, "rest", "api", "2", "issue", key, "comment"), &tail); err != nil {
			c.log.Warn("jira: fetch latest comments", "key", key, "err", err)
		} else {
			list = tail.Comments
		}
	}
	if len(list) > c.commentsLimit {
		list = list[len(list)-c.commentsLimit:]
	}
	out := make([]Comment, 0, len(list))
	for _, cm := range list {
		author := cm.Author.DisplayName
		if author == "" {
			author = cm.Author.Name
		}
		out = append(out, Comment{Author: oneLine(author), Created: parseTime(cm.Created), Body: truncate(cm.Body)})
	}
	return out
}

func str(m json.RawMessage) string {
	var s string
	if len(m) == 0 || json.Unmarshal(m, &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

func nameOf(m json.RawMessage) string {
	var n named
	if len(m) == 0 || json.Unmarshal(m, &n) != nil {
		return ""
	}
	return oneLine(n.Name)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= MaxTextRunes {
		return s
	}
	return string(r[:MaxTextRunes]) + "\n…(обрезано)"
}

// parseTime parses Jira timestamps ("2026-10-24T10:00:00.000+0300").
func parseTime(s string) time.Time {
	for _, layout := range []string{"2006-01-02T15:04:05.000-0700", time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
