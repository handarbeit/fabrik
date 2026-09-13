package githubauth

import "sort"

// permissionOrdinal ranks a GitHub App permission level so shortfall
// detection can compare levels correctly. GitHub's permission levels are
// cumulative — "write" implies "read", "admin" implies "write" and "read" —
// so a plain string-equality comparison against a required level would
// false-positive whenever a scope is granted at a higher level than
// required (e.g. "issues: admin" would look like a shortfall against a
// required "issues: write"). An unrecognized or empty level ordinals to 0,
// same as "none" — i.e. "not sufficient for any real requirement" — which
// is the conservative, fail-loud choice for a level GitHub's API returns
// that this package doesn't yet recognize.
func permissionOrdinal(level string) int {
	switch level {
	case "read":
		return 1
	case "write":
		return 2
	case "admin":
		return 3
	default:
		return 0
	}
}

// RequiredPermissionShortfall describes one permission the caller's code
// requires that an installation's actually-granted permissions don't meet —
// either because the permission is absent entirely (Granted == "") or
// granted at a lower level than required.
type RequiredPermissionShortfall struct {
	Permission string
	Required   string
	Granted    string
}

// checkGrantedPermissions compares granted (an installation's actual,
// GET-/app/installations[/{id}]-reported permissions — never GET /app's
// merely-requested ones, see AppInstallation.Permissions) against required
// (the caller's own required-permission set — e.g. PrueferRequiredPermissions)
// and returns every shortfall found, sorted by permission name for
// deterministic output. A permission required is not itself parameterized
// by this function — required is entirely caller-supplied, so a future
// second caller (e.g. #770's engine identity) passes its own set without
// this package hardcoding anyone's specific scopes.
//
// This is inherently a hand-maintained correspondence: if a caller's code
// starts using a new GitHub API needing a permission not yet added to its
// required set, this check will pass while a genuinely new gap goes
// undetected. Keeping the required set in sync with actual API usage is the
// caller's responsibility (see manifest.go's PrueferRequiredPermissions,
// which is the single source of truth for Pruefer's own set).
func checkGrantedPermissions(granted, required map[string]string) []RequiredPermissionShortfall {
	var shortfalls []RequiredPermissionShortfall
	for perm, reqLevel := range required {
		gotLevel := granted[perm]
		if permissionOrdinal(gotLevel) < permissionOrdinal(reqLevel) {
			shortfalls = append(shortfalls, RequiredPermissionShortfall{
				Permission: perm,
				Required:   reqLevel,
				Granted:    gotLevel,
			})
		}
	}
	sort.Slice(shortfalls, func(i, j int) bool {
		return shortfalls[i].Permission < shortfalls[j].Permission
	})
	return shortfalls
}

// logPermissionShortfalls logs one loud, "!"-prefixed line per shortfall,
// naming the installation, the specific permission, the level required, and
// the level actually granted — AC2's literal requirement, so a scope
// mismatch surfaces as an explicit log line instead of a silent 403 the
// first time the feature exercising that permission actually runs. A
// missing permission (Granted == "") is reported as "granted: none" rather
// than an empty string, which would otherwise read as a blank/confusing
// value in the log line.
func logPermissionShortfalls(installationID int64, account string, shortfalls []RequiredPermissionShortfall, logf func(format string, args ...any)) {
	for _, s := range shortfalls {
		granted := s.Granted
		if granted == "" {
			granted = "none"
		}
		logf("! installation %d (%s): permission %q is granted %q but %q is required — some features will fail (403) the first time they're used; raise the App's permission and approve it on this installation to fix", installationID, account, s.Permission, granted, s.Required)
	}
}
