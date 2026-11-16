package engine

import (
	"github.com/handarbeit/fabrik/stages"
)

// ReviewStatusEnabled reports whether the `fabrik/review` commit status (#2471,
// ADR 2471) is enabled anywhere: the repo-wide review_status config key, or any
// stage's own review_status: true. It is the one decision the startup
// permission check and `fabrik init --github-app` share, so they cannot
// disagree about when `statuses: write` is demanded. It deliberately ignores
// per-item labels and wait_for_reviews: a mis-set stage is reported separately,
// and over-asking for the grant is the safe direction.
func ReviewStatusEnabled(repoWide bool, stgs []*stages.Stage) bool {
	if repoWide {
		return true
	}
	for _, s := range stgs {
		if s != nil && s.ReviewStatus != nil && *s.ReviewStatus {
			return true
		}
	}
	return false
}
