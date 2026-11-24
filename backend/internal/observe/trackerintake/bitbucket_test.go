package trackerintake

import (
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestTrackerRepoBitbucketOriginIsUnsupported(t *testing.T) {
	for _, origin := range []string{"git@bitbucket.org:dealhubio/aura-sre.git", "https://bitbucket.org/dealhubio/aura-sre"} {
		project := domain.ProjectRecord{RepoOriginURL: origin}
		if repo, ok := trackerRepo(project, domain.TrackerIntakeConfig{Enabled: true, Assignee: "*"}); ok {
			t.Fatalf("%s: got tracker repo %+v, want unsupported", origin, repo)
		}
	}
}
