// Package bitbucket is the Bitbucket Cloud SCM adapter for AO.
//
// It talks to the Bitbucket Cloud REST API 2.0 (https://api.bitbucket.org/2.0)
// with HTTP basic auth (username + API token). Bitbucket Server / Data Center
// is not supported.
//
// # Credentials
//
// Credentials resolve, in order, from AO_BITBUCKET_USERNAME /
// AO_BITBUCKET_API_TOKEN, then BITBUCKET_USERNAME / BITBUCKET_API_TOKEN, then
// the macOS Keychain generic password with service "ao-bitbucket" and the
// username as account. Tokens are never logged or included in errors.
//
// # Remotes
//
// ParseRepository accepts only the bitbucket.org host (scp-style, ssh:// and
// http(s):// remotes). The workspace and repository slug are lower-cased
// because Bitbucket treats them case-insensitively. SCMRepo follows the GitLab
// adapter's convention: Owner is the workspace, Name the slug and Repo
// "<workspace>/<slug>".
package bitbucket
