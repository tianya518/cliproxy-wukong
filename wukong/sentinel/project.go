package sentinel

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

const (
	projectsSidebarPath = "/backend-api/gizmos/snorlax/sidebar"
	projectsCreatePath  = "/backend-api/projects"
	maxProjectPages     = 20
	maxProjectConvPages = 50
)

// Project 是 ChatGPT「项目」（snorlax gizmo，id 形如 g-p-…）。
type Project struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	ShortURL     string `json:"short_url,omitempty"`
	GizmoType    string `json:"gizmo_type,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

// ProjectConversation 是项目内的一条会话摘要。
type ProjectConversation struct {
	ID                     string `json:"id"`
	Title                  string `json:"title"`
	GizmoID                string `json:"gizmo_id,omitempty"`
	ConversationTemplateID string `json:"conversation_template_id,omitempty"`
	CreateTime             string `json:"create_time,omitempty"`
	UpdateTime             string `json:"update_time,omitempty"`
}

type rawGizmo struct {
	ID           string `json:"id"`
	ShortURL     string `json:"short_url"`
	Instructions string `json:"instructions"`
	GizmoType    string `json:"gizmo_type"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	Display      struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"display"`
}

func (g rawGizmo) project() Project {
	return Project{
		ID:           g.ID,
		Name:         g.Display.Name,
		Description:  g.Display.Description,
		Instructions: g.Instructions,
		ShortURL:     g.ShortURL,
		GizmoType:    g.GizmoType,
		CreatedAt:    g.CreatedAt,
		UpdatedAt:    g.UpdatedAt,
	}
}

// NormalizeGizmoID 把用户传入的项目 id / 网址收成官网 gizmo id（g-p-… / g-…）。
func NormalizeGizmoID(raw string) string {
	id := strings.TrimSpace(raw)
	if id == "" {
		return ""
	}
	if i := strings.Index(id, "/g/"); i >= 0 {
		rest := id[i+3:]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			rest = rest[:slash]
		}
		id = rest
	}
	id = strings.TrimSpace(id)
	if strings.HasPrefix(id, "g-") {
		return id
	}
	return "g-p-" + id
}

// ListProjects 列出账号下的 ChatGPT 项目（跟官网侧栏同一份 gizmos/snorlax/sidebar）。
func (c *Client) ListProjects() ([]Project, error) {
	var out []Project
	cursor := ""
	for page := 0; page < maxProjectPages; page++ {
		q := url.Values{}
		q.Set("conversations_per_gizmo", "0")
		q.Set("owned_only", "true")
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		apiPath := projectsSidebarPath + "?" + q.Encode()
		resp, err := c.httpClient.R().
			SetHeaders(map[string]string{
				"Accept":                "*/*",
				"x-openai-target-path":  projectsSidebarPath,
				"x-openai-target-route": projectsSidebarPath,
			}).
			Get(apiPath)
		if err != nil {
			return nil, fmt.Errorf("list projects: %w", err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("list projects %d: %s", resp.StatusCode, truncateStr(resp.String(), 300))
		}
		items, next, err := parseSidebarProjects(resp.Bytes())
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if next == "" {
			break
		}
		cursor = next
	}
	c.logf("[project] listed %d", len(out))
	return out, nil
}

// GetProject 拉取单个项目详情。id 可以是 g-p-… 或裸 hex。
func (c *Client) GetProject(id string) (*Project, error) {
	gid := NormalizeGizmoID(id)
	if gid == "" {
		return nil, fmt.Errorf("project id is required")
	}
	apiPath := "/backend-api/gizmos/" + gid
	resp, err := c.httpClient.R().
		SetHeaders(map[string]string{
			"Accept":                "*/*",
			"x-openai-target-path":  apiPath,
			"x-openai-target-route": "/backend-api/gizmos/{gizmo_id}",
		}).
		Get(apiPath)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("get project %d: %s", resp.StatusCode, truncateStr(resp.String(), 300))
	}
	p, err := parseGizmoProject(resp.Bytes())
	if err != nil {
		return nil, err
	}
	return p, nil
}

// CreateProject 在官网新建一个项目。
//
// 2026-08-26 实抓：POST /backend-api/projects {"name","instructions"}
// 响应：{"resource":{"gizmo":{...}},"error":null,"sharing_targets":...}
func (c *Client) CreateProject(name, instructions string) (*Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("project name is required")
	}
	resp, err := c.httpClient.R().
		SetHeaders(map[string]string{
			"Content-Type":          "application/json",
			"Accept":                "*/*",
			"x-openai-target-path":  projectsCreatePath,
			"x-openai-target-route": projectsCreatePath,
		}).
		SetBody(map[string]string{
			"name":         name,
			"instructions": instructions,
		}).
		Post(projectsCreatePath)
	if err != nil {
		return nil, fmt.Errorf("create project: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("create project %d: %s", resp.StatusCode, truncateStr(resp.String(), 300))
	}
	p, err := parseCreateProject(resp.Bytes())
	if err != nil {
		return nil, err
	}
	c.logf("[project] created id=%s name=%s", p.ID, p.Name)
	return p, nil
}

// ListProjectConversations 列出项目内的会话。
func (c *Client) ListProjectConversations(id string) ([]ProjectConversation, error) {
	gid := NormalizeGizmoID(id)
	if gid == "" {
		return nil, fmt.Errorf("project id is required")
	}
	var out []ProjectConversation
	cursor := ""
	for page := 0; page < maxProjectConvPages; page++ {
		apiPath := "/backend-api/gizmos/" + gid + "/conversations"
		req := c.httpClient.R().SetHeaders(map[string]string{
			"Accept":                "*/*",
			"x-openai-target-path":  apiPath,
			"x-openai-target-route": "/backend-api/gizmos/{gizmo_id}/conversations",
		})
		if cursor != "" {
			req.SetQueryParam("cursor", cursor)
		}
		resp, err := req.Get(apiPath)
		if err != nil {
			return nil, fmt.Errorf("list project conversations: %w", err)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("list project conversations %d: %s", resp.StatusCode, truncateStr(resp.String(), 300))
		}
		items, next, err := parseProjectConversations(resp.Bytes())
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if next == "" {
			break
		}
		cursor = next
	}
	c.logf("[project] conversations id=%s n=%d", gid, len(out))
	return out, nil
}

func parseSidebarProjects(raw []byte) ([]Project, string, error) {
	var page struct {
		Items []struct {
			Gizmo struct {
				Gizmo rawGizmo `json:"gizmo"`
			} `json:"gizmo"`
		} `json:"items"`
		Cursor json.RawMessage `json:"cursor"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, "", fmt.Errorf("decode project sidebar: %w", err)
	}
	out := make([]Project, 0, len(page.Items))
	for _, it := range page.Items {
		if it.Gizmo.Gizmo.ID == "" {
			continue
		}
		out = append(out, it.Gizmo.Gizmo.project())
	}
	return out, decodeCursor(page.Cursor), nil
}

func parseCreateProject(raw []byte) (*Project, error) {
	var resp struct {
		Resource struct {
			Gizmo rawGizmo `json:"gizmo"`
		} `json:"resource"`
		Gizmo rawGizmo        `json:"gizmo"`
		ID    string          `json:"id"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode create project: %w", err)
	}
	g := resp.Resource.Gizmo
	if g.ID == "" {
		g = resp.Gizmo
	}
	if g.ID == "" && resp.ID != "" {
		g.ID = resp.ID
	}
	if g.ID == "" {
		if msg := decodeAPIError(resp.Error); msg != "" {
			return nil, fmt.Errorf("create project: %s", msg)
		}
		return nil, fmt.Errorf("create project: empty gizmo id: %s", truncateStr(string(raw), 240))
	}
	p := g.project()
	return &p, nil
}

func parseGizmoProject(raw []byte) (*Project, error) {
	var resp struct {
		Gizmo rawGizmo `json:"gizmo"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode project: %w", err)
	}
	if resp.Gizmo.ID == "" {
		var direct rawGizmo
		if err := json.Unmarshal(raw, &direct); err == nil && direct.ID != "" {
			p := direct.project()
			return &p, nil
		}
		return nil, fmt.Errorf("decode project: empty gizmo id")
	}
	p := resp.Gizmo.project()
	return &p, nil
}

func parseProjectConversations(raw []byte) ([]ProjectConversation, string, error) {
	var page struct {
		Items  []ProjectConversation `json:"items"`
		Cursor json.RawMessage       `json:"cursor"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, "", fmt.Errorf("decode project conversations: %w", err)
	}
	return page.Items, decodeCursor(page.Cursor), nil
}

func decodeCursor(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

func decodeAPIError(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) == nil {
		if m, ok := obj["message"].(string); ok && m != "" {
			return m
		}
		if d, ok := obj["detail"].(string); ok && d != "" {
			return d
		}
	}
	return truncateStr(string(raw), 200)
}
