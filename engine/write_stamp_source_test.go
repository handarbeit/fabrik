package engine

import (
	gh "github.com/handarbeit/fabrik/github"
)

// The production client must implement WriteStampSource: observeSelfWrite does a
// runtime type assertion, so losing the methods (a rename, a signature change)
// would silently stop anchoring the self-write baseline (#1945) and cost a
// redundant deep fetch after every engine write. Fail the build instead.
var _ WriteStampSource = (*gh.Client)(nil)
