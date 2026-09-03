// Package ownerid answers the one question about a CODEOWNERS owner that only
// GitHub can answer: does it resolve to anything?
//
// It exists because two callers now ask it and they act on the answer in
// opposite directions. `lint` asks so it can DELETE an owner that is gone;
// `sync` asks (R-41) so it can REFUSE to write one. A false "gone" costs the
// first caller a live team and the second caller a halted rollout, so both
// need the same fail-closed discipline — and a second copy of that discipline
// is how the two drift apart, which is the reasoning ops.FoldOwner already
// records for owner identity.
//
// Every answer is definitive or an error (R-12): "the lookup could not be
// answered" is never rendered as a negative.
package ownerid

import (
	"errors"
	"fmt"
	"strings"

	"github.com/jordonpeterson/codeowners-tool/internal/ghapi"
)

// Verifier is the slice of the GitHub API this question needs.
type Verifier interface {
	// ProbeOrg proves the token can enumerate the org. Until it succeeds, an
	// org-scoped 404 does not mean "gone" (R-12).
	ProbeOrg(org string) error
	// TeamExists reports whether @org/slug exists. Call ProbeOrg first.
	TeamExists(org, slug string) (bool, error)
	// ViewerIsOrgAdmin reports whether the token is an OWNER of org. Only an
	// owner sees secret teams, so only an owner's team-404 means "deleted"
	// rather than "invisible to me".
	ViewerIsOrgAdmin(org string) (bool, error)
	// UserExists reports whether @login exists.
	UserExists(login string) (bool, error)
}

// IsGone reports whether owner definitively does not exist. An error means the
// question could not be answered (R-12).
// LOOKUPS ARE CASE-FOLDED, identity is not.
//
// GitHub treats a login and an org/team handle case-insensitively, and team
// slugs are lowercase by construction — so `@Org/Team` and `@org/team` are one
// owner. Asking the API about the mixed-case spelling risks a 404 that means
// nothing more than "you typed it differently", and a 404 here either DELETES
// an owner (lint) or halts a rollout (R-41). Folding the lookup is strictly
// safer: if the API is case-insensitive it changes nothing, and if it is not,
// it prevents a live team being stripped because somebody capitalised it. The
// file's own bytes are never rewritten from this — R-38b: folding governs
// matching, never output.
func IsGone(v Verifier, owner string) (gone bool, reason string, err error) {
	owner = strings.ToLower(owner)
	if org, slug, isTeam := SplitTeam(owner); isTeam {
		// A team 404 is only meaningful once the token has proven it can
		// enumerate the org — otherwise "invisible to these scopes" and
		// "deleted" are the same response.
		if err := v.ProbeOrg(org); err != nil {
			return false, "", err
		}
		exists, err := v.TeamExists(org, slug)
		if err != nil {
			return false, "", err
		}
		if exists {
			return false, "", nil
		}
		// A 404, which is where reporting and ACTING part company. GitHub
		// returns exactly this for a team that was removed and for a SECRET
		// team the caller cannot see, and ProbeOrg does not separate them: it
		// proves the token can call the endpoint, not that it can see every
		// team behind it. Only an org owner sees secret teams, so only an org
		// owner's 404 is definitive. Anything else is inconclusive — which is
		// R-12 doing its job, and costs nothing on the common path, because
		// this is only ever reached when a team already looks gone.
		admin, err := v.ViewerIsOrgAdmin(org)
		if err != nil {
			return false, "", err
		}
		if !admin {
			return false, "", &ghapi.Inconclusive{Reason: fmt.Sprintf(
				"%s was not found in %s, but this token is not an owner of that org, and a secret team returns the same 404 as a deleted one — re-run with an org-owner token to have this decided, or remove the owner by hand",
				owner, org)}
		}
		return true, fmt.Sprintf("team %s does not exist (deleted or renamed); review requests to it silently do nothing", owner), nil
	}
	exists, err := v.UserExists(strings.TrimPrefix(owner, "@"))
	if err != nil {
		return false, "", err
	}
	if exists {
		return false, "", nil
	}
	return true, fmt.Sprintf("user %s does not exist (deleted or renamed); review requests to it silently do nothing", owner), nil
}

// SplitTeam splits an @org/team owner into its two halves. A bare @login and
// an email owner are not teams.
func SplitTeam(owner string) (org, slug string, ok bool) {
	if !strings.HasPrefix(owner, "@") || !strings.Contains(owner, "/") {
		return "", "", false
	}
	parts := strings.SplitN(strings.TrimPrefix(owner, "@"), "/", 2)
	return parts[0], parts[1], true
}

// Reason renders an error as the sentence a message can carry: an
// *ghapi.Inconclusive's own reason, or the raw error for a transport failure.
func Reason(err error) string {
	var inc *ghapi.Inconclusive
	if errors.As(err, &inc) {
		return inc.Reason
	}
	return err.Error()
}
