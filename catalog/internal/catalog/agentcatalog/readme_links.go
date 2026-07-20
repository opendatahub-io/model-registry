package agentcatalog

import (
	"net/url"
	"regexp"
	"strings"
)

// mdImageRe matches markdown image syntax: ![alt text](url)
var mdImageRe = regexp.MustCompile(`(!\[[^\]]*\]\()([^)]+)(\))`)

// mdLinkRe matches markdown link syntax: [text](url)
var mdLinkRe = regexp.MustCompile(`(\[[^\]]*\]\()([^)]+)(\))`)

// htmlImgRe matches HTML <img> tags with a src attribute using either single or double quotes.
var htmlImgRe = regexp.MustCompile(`(?i)(<img\s+(?:[^>]*?\s)?src\s*=\s*)(["'])([^"']*?)(\2)([^>]*?>)`)

// resolveReadmeLinks rewrites relative URLs in readme markdown content to absolute
// GitHub raw content URLs. It processes both markdown image/link syntax and HTML
// <img> tags with relative src attributes.
//
// The repositoryUrl is expected to be a GitHub URL like:
//
//	https://github.com/owner/repo
//	https://github.com/owner/repo/tree/main
//	https://github.com/owner/repo/tree/main/path/to/dir
//
// Relative paths starting with "/" are resolved from the repository root.
// Other relative paths are resolved from the directory specified in repositoryUrl.
// Absolute URLs (http://, https://, data:) are left unchanged.
func resolveReadmeLinks(readme string, repositoryUrl string) string {
	if readme == "" || repositoryUrl == "" {
		return readme
	}

	owner, repo, branch, subPath := parseGitHubURL(repositoryUrl)
	if owner == "" || repo == "" {
		return readme
	}

	rawBase := "https://raw.githubusercontent.com/" + owner + "/" + repo + "/" + branch

	// Process markdown images: ![alt](relative/path)
	readme = mdImageRe.ReplaceAllStringFunc(readme, func(match string) string {
		parts := mdImageRe.FindStringSubmatch(match)
		if len(parts) < 4 {
			return match
		}
		resolved := resolveURL(parts[2], rawBase, subPath)
		return parts[1] + resolved + parts[3]
	})

	// Process markdown links: [text](relative/path)
	// Only process links that are not image links (not preceded by !)
	readme = processMarkdownLinks(readme, rawBase, subPath)

	// Process HTML img tags: <img src="relative/path">
	readme = htmlImgRe.ReplaceAllStringFunc(readme, func(match string) string {
		parts := htmlImgRe.FindStringSubmatch(match)
		if len(parts) < 6 {
			return match
		}
		resolved := resolveURL(parts[3], rawBase, subPath)
		return parts[1] + parts[2] + resolved + parts[4] + parts[5]
	})

	return readme
}

// processMarkdownLinks resolves relative URLs in markdown links [text](url),
// skipping markdown images which are already handled by mdImageRe.
func processMarkdownLinks(readme string, rawBase string, subPath string) string {
	result := readme
	// Find all markdown link matches
	matches := mdLinkRe.FindAllStringSubmatchIndex(result, -1)
	if len(matches) == 0 {
		return result
	}

	// Process matches in reverse order to maintain correct indices
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		fullStart := m[0]

		// Skip if preceded by '!' (markdown image, already handled)
		if fullStart > 0 && result[fullStart-1] == '!' {
			continue
		}

		// Extract the URL part (group 2)
		urlStart := m[4]
		urlEnd := m[5]
		originalURL := result[urlStart:urlEnd]

		resolved := resolveURL(originalURL, rawBase, subPath)
		if resolved != originalURL {
			result = result[:urlStart] + resolved + result[urlEnd:]
		}
	}

	return result
}

// resolveURL resolves a potentially relative URL against the GitHub raw base URL.
// Absolute URLs (http://, https://, data:, mailto:, #) are returned unchanged.
func resolveURL(rawURL string, rawBase string, subPath string) string {
	trimmed := strings.TrimSpace(rawURL)

	// Skip absolute URLs and special schemes
	if strings.HasPrefix(trimmed, "http://") ||
		strings.HasPrefix(trimmed, "https://") ||
		strings.HasPrefix(trimmed, "data:") ||
		strings.HasPrefix(trimmed, "mailto:") ||
		strings.HasPrefix(trimmed, "#") {
		return rawURL
	}

	if strings.HasPrefix(trimmed, "/") {
		// Absolute path from repo root: /images/foo.png
		return rawBase + trimmed
	}

	// Relative path: images/foo.png — resolve from the subPath directory
	if subPath != "" {
		return rawBase + "/" + subPath + "/" + trimmed
	}
	return rawBase + "/" + trimmed
}

// parseGitHubURL extracts the owner, repo, branch, and sub-path from a GitHub URL.
// It handles URLs like:
//
//	https://github.com/owner/repo
//	https://github.com/owner/repo/tree/main
//	https://github.com/owner/repo/tree/main/path/to/dir
//
// Returns empty strings if the URL is not a recognized GitHub URL.
func parseGitHubURL(rawURL string) (owner, repo, branch, subPath string) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", "", ""
	}

	if u.Host != "github.com" {
		return "", "", "", ""
	}

	// Split the path: /owner/repo[/tree/branch[/subpath...]]
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return "", "", "", ""
	}

	owner = parts[0]
	repo = parts[1]
	branch = "main" // default

	if len(parts) >= 4 && parts[2] == "tree" {
		branch = parts[3]
		if len(parts) > 4 {
			subPath = strings.Join(parts[4:], "/")
		}
	}

	return owner, repo, branch, subPath
}
