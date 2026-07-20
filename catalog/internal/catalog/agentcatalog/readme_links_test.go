package agentcatalog

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseGitHubURL(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
		wantBranch string
		wantSub   string
	}{
		{
			name:       "simple repo URL",
			url:        "https://github.com/owner/repo",
			wantOwner:  "owner",
			wantRepo:   "repo",
			wantBranch: "main",
			wantSub:    "",
		},
		{
			name:       "repo with tree and branch",
			url:        "https://github.com/owner/repo/tree/main",
			wantOwner:  "owner",
			wantRepo:   "repo",
			wantBranch: "main",
			wantSub:    "",
		},
		{
			name:       "repo with tree, branch, and subpath",
			url:        "https://github.com/owner/repo/tree/main/path/to/agent",
			wantOwner:  "owner",
			wantRepo:   "repo",
			wantBranch: "main",
			wantSub:    "path/to/agent",
		},
		{
			name:       "repo with non-main branch",
			url:        "https://github.com/owner/repo/tree/develop/agents/my-agent",
			wantOwner:  "owner",
			wantRepo:   "repo",
			wantBranch: "develop",
			wantSub:    "agents/my-agent",
		},
		{
			name:       "non-GitHub URL",
			url:        "https://gitlab.com/owner/repo",
			wantOwner:  "",
			wantRepo:   "",
			wantBranch: "",
			wantSub:    "",
		},
		{
			name:       "invalid URL",
			url:        "not a url",
			wantOwner:  "",
			wantRepo:   "",
			wantBranch: "",
			wantSub:    "",
		},
		{
			name:       "GitHub URL with only owner",
			url:        "https://github.com/owner",
			wantOwner:  "",
			wantRepo:   "",
			wantBranch: "",
			wantSub:    "",
		},
		{
			name:       "trailing slash",
			url:        "https://github.com/owner/repo/",
			wantOwner:  "owner",
			wantRepo:   "repo",
			wantBranch: "main",
			wantSub:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, repo, branch, sub := parseGitHubURL(tt.url)
			assert.Equal(t, tt.wantOwner, owner)
			assert.Equal(t, tt.wantRepo, repo)
			assert.Equal(t, tt.wantBranch, branch)
			assert.Equal(t, tt.wantSub, sub)
		})
	}
}

func TestResolveURL(t *testing.T) {
	rawBase := "https://raw.githubusercontent.com/owner/repo/main"

	tests := []struct {
		name    string
		rawURL  string
		subPath string
		want    string
	}{
		{
			name:    "absolute http URL unchanged",
			rawURL:  "http://example.com/image.png",
			subPath: "",
			want:    "http://example.com/image.png",
		},
		{
			name:    "absolute https URL unchanged",
			rawURL:  "https://example.com/image.png",
			subPath: "",
			want:    "https://example.com/image.png",
		},
		{
			name:    "data URI unchanged",
			rawURL:  "data:image/png;base64,abc",
			subPath: "",
			want:    "data:image/png;base64,abc",
		},
		{
			name:    "mailto link unchanged",
			rawURL:  "mailto:test@example.com",
			subPath: "",
			want:    "mailto:test@example.com",
		},
		{
			name:    "anchor link unchanged",
			rawURL:  "#section-heading",
			subPath: "",
			want:    "#section-heading",
		},
		{
			name:    "absolute path from root",
			rawURL:  "/images/architecture.png",
			subPath: "",
			want:    "https://raw.githubusercontent.com/owner/repo/main/images/architecture.png",
		},
		{
			name:    "relative path without subpath",
			rawURL:  "images/architecture.png",
			subPath: "",
			want:    "https://raw.githubusercontent.com/owner/repo/main/images/architecture.png",
		},
		{
			name:    "relative path with subpath",
			rawURL:  "images/architecture.png",
			subPath: "agents/my-agent",
			want:    "https://raw.githubusercontent.com/owner/repo/main/agents/my-agent/images/architecture.png",
		},
		{
			name:    "absolute path ignores subpath",
			rawURL:  "/images/architecture.png",
			subPath: "agents/my-agent",
			want:    "https://raw.githubusercontent.com/owner/repo/main/images/architecture.png",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveURL(tt.rawURL, rawBase, tt.subPath)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestResolveReadmeLinks(t *testing.T) {
	tests := []struct {
		name          string
		readme        string
		repositoryUrl string
		want          string
	}{
		{
			name:          "empty readme",
			readme:        "",
			repositoryUrl: "https://github.com/owner/repo",
			want:          "",
		},
		{
			name:          "empty repositoryUrl",
			readme:        "![alt](/images/foo.png)",
			repositoryUrl: "",
			want:          "![alt](/images/foo.png)",
		},
		{
			name:          "non-GitHub repositoryUrl",
			readme:        "![alt](/images/foo.png)",
			repositoryUrl: "https://gitlab.com/owner/repo",
			want:          "![alt](/images/foo.png)",
		},
		{
			name:          "markdown image with absolute path",
			readme:        "![architecture](/images/architecture.png)",
			repositoryUrl: "https://github.com/owner/repo",
			want:          "![architecture](https://raw.githubusercontent.com/owner/repo/main/images/architecture.png)",
		},
		{
			name:          "markdown image with relative path",
			readme:        "![diagram](docs/diagram.png)",
			repositoryUrl: "https://github.com/owner/repo/tree/main/agents/my-agent",
			want:          "![diagram](https://raw.githubusercontent.com/owner/repo/main/agents/my-agent/docs/diagram.png)",
		},
		{
			name:          "markdown image with absolute URL unchanged",
			readme:        "![logo](https://example.com/logo.png)",
			repositoryUrl: "https://github.com/owner/repo",
			want:          "![logo](https://example.com/logo.png)",
		},
		{
			name:          "HTML img tag with double-quoted relative src",
			readme:        `<img src="/images/architecture.png" alt="architecture">`,
			repositoryUrl: "https://github.com/owner/repo",
			want:          `<img src="https://raw.githubusercontent.com/owner/repo/main/images/architecture.png" alt="architecture">`,
		},
		{
			name:          "HTML img tag with single-quoted relative src",
			readme:        `<img src='/images/architecture.png' alt='architecture'>`,
			repositoryUrl: "https://github.com/owner/repo",
			want:          `<img src='https://raw.githubusercontent.com/owner/repo/main/images/architecture.png' alt='architecture'>`,
		},
		{
			name:          "HTML img tag with absolute URL unchanged",
			readme:        `<img src="https://example.com/logo.png" alt="logo">`,
			repositoryUrl: "https://github.com/owner/repo",
			want:          `<img src="https://example.com/logo.png" alt="logo">`,
		},
		{
			name:          "HTML img tag with relative path no leading slash",
			readme:        `<img src="images/foo.png" alt="foo">`,
			repositoryUrl: "https://github.com/owner/repo/tree/main/agents/my-agent",
			want:          `<img src="https://raw.githubusercontent.com/owner/repo/main/agents/my-agent/images/foo.png" alt="foo">`,
		},
		{
			name:          "HTML img tag with additional attributes",
			readme:        `<img width="500" src="/images/arch.png" alt="arch" style="max-width: 100%;">`,
			repositoryUrl: "https://github.com/owner/repo",
			want:          `<img width="500" src="https://raw.githubusercontent.com/owner/repo/main/images/arch.png" alt="arch" style="max-width: 100%;">`,
		},
		{
			name: "multiple img tags and markdown images",
			readme: `# Agent README

![overview](/images/overview.png)

Some text here.

<img src="/images/architecture.png" alt="architecture" width="500">

More text.

<img src="/images/flow.png" alt="flow">

[Link to docs](https://example.com/docs)`,
			repositoryUrl: "https://github.com/owner/repo/tree/main",
			want: `# Agent README

![overview](https://raw.githubusercontent.com/owner/repo/main/images/overview.png)

Some text here.

<img src="https://raw.githubusercontent.com/owner/repo/main/images/architecture.png" alt="architecture" width="500">

More text.

<img src="https://raw.githubusercontent.com/owner/repo/main/images/flow.png" alt="flow">

[Link to docs](https://example.com/docs)`,
		},
		{
			name:          "markdown link with relative path",
			readme:        "[docs](docs/README.md)",
			repositoryUrl: "https://github.com/owner/repo/tree/main/agents/my-agent",
			want:          "[docs](https://raw.githubusercontent.com/owner/repo/main/agents/my-agent/docs/README.md)",
		},
		{
			name:          "markdown link with absolute URL unchanged",
			readme:        "[docs](https://example.com/docs)",
			repositoryUrl: "https://github.com/owner/repo",
			want:          "[docs](https://example.com/docs)",
		},
		{
			name:          "markdown link with anchor unchanged",
			readme:        "[section](#features)",
			repositoryUrl: "https://github.com/owner/repo",
			want:          "[section](#features)",
		},
		{
			name:          "HTML img with self-closing tag",
			readme:        `<img src="/images/logo.svg" />`,
			repositoryUrl: "https://github.com/owner/repo",
			want:          `<img src="https://raw.githubusercontent.com/owner/repo/main/images/logo.svg" />`,
		},
		{
			name:          "non-main branch",
			readme:        "![img](/images/test.png)",
			repositoryUrl: "https://github.com/owner/repo/tree/develop",
			want:          "![img](https://raw.githubusercontent.com/owner/repo/develop/images/test.png)",
		},
		{
			name:          "HTML img tag with spaces around equals",
			readme:        `<img src = "/images/test.png" alt="test">`,
			repositoryUrl: "https://github.com/owner/repo",
			want:          `<img src = "https://raw.githubusercontent.com/owner/repo/main/images/test.png" alt="test">`,
		},
		{
			name:          "HTML img tag with data-src should not match data-src attribute",
			readme:        `<img data-src="/images/lazy.png" src="/images/real.png" alt="test">`,
			repositoryUrl: "https://github.com/owner/repo",
			want:          `<img data-src="/images/lazy.png" src="https://raw.githubusercontent.com/owner/repo/main/images/real.png" alt="test">`,
		},
		{
			name:          "HTML img tag with only data-src should remain unchanged",
			readme:        `<img data-src="/images/lazy.png" alt="test">`,
			repositoryUrl: "https://github.com/owner/repo",
			want:          `<img data-src="/images/lazy.png" alt="test">`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveReadmeLinks(tt.readme, tt.repositoryUrl)
			assert.Equal(t, tt.want, got)
		})
	}
}
