package bitbucket

import (
	"net/url"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const (
	providerName  = "bitbucket"
	bitbucketHost = "bitbucket.org"
)

var defaultPorts = map[string]string{"ssh": "22", "http": "80", "https": "443"}

// ParseRepository parses a Git remote URL and returns an SCMRepo when it points
// at a bitbucket.org repository. Only the exact host bitbucket.org is accepted
// so credentials are never attached to look-alike hosts.
//
// Supported formats:
//
//   - scp-style: git@bitbucket.org:workspace/repo.git
//   - SSH:       ssh://git@bitbucket.org/workspace/repo.git
//   - HTTPS:     https://user@bitbucket.org/workspace/repo.git
//
// A trailing slash and ".git" are stripped. Workspace and slug are lower-cased
// because Bitbucket treats them case-insensitively.
func (p *Provider) ParseRepository(remote string) (ports.SCMRepo, bool) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ports.SCMRepo{}, false
	}

	var host, path string
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil {
			return ports.SCMRepo{}, false
		}
		switch u.Scheme {
		case "ssh", "http", "https":
		default:
			return ports.SCMRepo{}, false
		}
		// Bitbucket Cloud serves only the scheme's default port; anything else
		// is a different server, so credentials must not be sent to it.
		if port := u.Port(); port != "" && port != defaultPorts[u.Scheme] {
			return ports.SCMRepo{}, false
		}
		host, path = u.Hostname(), u.Path
	} else {
		// scp-style: [user@]host:path
		hostPart, rest, ok := strings.Cut(remote, ":")
		if !ok {
			return ports.SCMRepo{}, false
		}
		if _, h, hasUser := strings.Cut(hostPart, "@"); hasUser {
			hostPart = h
		}
		host, path = hostPart, rest
	}
	if !strings.EqualFold(host, bitbucketHost) {
		return ports.SCMRepo{}, false
	}

	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ports.SCMRepo{}, false
	}
	owner, name := strings.ToLower(parts[0]), strings.ToLower(parts[1])
	return ports.SCMRepo{
		Provider: providerName,
		Host:     bitbucketHost,
		Owner:    owner,
		Name:     name,
		Repo:     owner + "/" + name,
	}, true
}
