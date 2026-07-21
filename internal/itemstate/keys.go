package itemstate

import (
	"strconv"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
)

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func removeReviewRequest(rrs []gh.ReviewRequest, login string) []gh.ReviewRequest {
	out := rrs[:0:0]
	for _, rr := range rrs {
		if rr.Login != login {
			out = append(out, rr)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// unionStrings returns a slice containing all elements of existing plus any
// elements from incoming that are not already present. Order is preserved.
func unionStrings(existing, incoming []string) []string {
	result := copyStrings(existing)
	for _, s := range incoming {
		if !containsString(result, s) {
			result = append(result, s)
		}
	}
	return result
}

func removeString(ss []string, s string) []string {
	out := ss[:0:0]
	for _, v := range ss {
		if v != s {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// prKeyFor constructs the PR reverse-index key "owner/repo#pr<N>".
// Same semantics as boardcache.prKey; duplicated here to keep the Store
// package self-contained (boardcache is a separate package).
func prKeyFor(repo string, prNum int) string {
	return repo + "#pr" + strconv.Itoa(prNum)
}

// parseKey splits "owner/repo#N" into repo and number.
func parseKey(key string) (string, int) {
	idx := strings.LastIndex(key, "#")
	if idx < 0 {
		return key, 0
	}
	repo := key[:idx]
	numStr := key[idx+1:]
	n := 0
	for _, c := range numStr {
		if c < '0' || c > '9' {
			return repo, 0
		}
		n = n*10 + int(c-'0')
	}
	return repo, n
}
