package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jordonpeterson/codeowners-tool/internal/ghapi"

	"github.com/jordonpeterson/codeowners-tool/internal/file"
	"github.com/jordonpeterson/codeowners-tool/internal/ops"
	"github.com/jordonpeterson/codeowners-tool/internal/ownerid"
)

// R-41: owner existence, asked BEFORE the write rather than after it.
//
// Everything else in this tool proves its claim against the repository's own
// files. That proof is complete for the question it answers — which paths a
// rule governs — and silent about the other half of a CODEOWNERS line: GitHub
// resolves an owner it does not recognise to nobody, without an error, so a
// rule naming a renamed team is written, reported `proven: tree`, and owns
// nothing. A wave that spelled one team wrong reached 100 repositories that
// way, and `audit` — which does ask — only runs when somebody has a reason to
// suspect the file, which is exactly what a clean exit 0 removes.
//
// The check is opt-in because the alternative is worse: making every `sync`
// require a token would break the offline runs the fleet verbs were designed
// for, and past 1.0.0 it would move failures between exit classes for
// policies that are working today. It is opt-in in two spellings — the flag,
// for a `--op` run, and `"verify_owners": true`, for the reviewed artifact a
// fleet runs a hundred times, where a guarantee that depends on every call
// site remembering a flag is not a guarantee.

// ownersIntroduced lists, in policy order and once each, every owner the run
// would put INTO FORCE. That is a narrower set than "every owner the ops
// mention", and the difference is deliberate:
//
//   - remove_owner's owners are not checked. Removing a team that was deleted
//     is the remedy this whole area recommends; verifying it would refuse the
//     one op that repairs the damage.
//   - rename_owner checks only the NEW name. The old one is on its way out,
//     and renaming away from a deleted team is the common case.
//   - set_owners with an empty list introduces nobody and asks nothing.
//
// Deduplication is by ops.FoldOwner: `@Org/Team` and `@org/team` are one
// owner (R-38a), so a 40-op baseline that spells a team both ways costs one
// lookup, not two — and cannot produce two contradictory verdicts.
func ownersIntroduced(list []ops.Op) []string {
	seen := map[string]bool{}
	var out []string
	add := func(o string) {
		if o == "" {
			return
		}
		k := ops.FoldOwner(o)
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, o)
	}
	for _, op := range list {
		switch op.Kind {
		case ops.AddOwner, ops.SetOwners:
			for _, o := range op.Owners {
				add(o)
			}
		case ops.RenameOwner:
			add(op.NewOwner)
		}
	}
	return out
}

// deadOwnersError is R-41's verdict when GitHub answered, definitively, that
// an owner the run would write does not exist. A policy error in the strict
// sense sync means by it: nothing about the repository decided it, so it is
// identical on every clone in the wave.
type deadOwnersError struct {
	reasons []string
	// mixed is true when an unanswerable lookup rode along with the dead
	// owners, which changes only the advice line — see verifyGuidance.
	mixed bool
}

func (e *deadOwnersError) Error() string     { return strings.Join(e.reasons, "; ") }
func (e *deadOwnersError) Reasons() []string { return e.reasons }

// inconclusiveOwnersError is R-12 reaching the writing verb: the lookup could
// not be answered, so the owner is neither proven live nor proven dead and
// nothing is written. It is NOT a policy error — the policy may be perfect
// and the token merely rate-limited — which is why it carries its own
// guidance line rather than "fix the policy, do not retry".
type inconclusiveOwnersError struct{ reasons []string }

func (e *inconclusiveOwnersError) Error() string     { return strings.Join(e.reasons, "; ") }
func (e *inconclusiveOwnersError) Reasons() []string { return e.reasons }

// inconclusiveGuidance is the remedy for the error above, and the reason
// exit3 takes a guidance line at all: sending an operator to edit a correct
// policy because their token was rate-limited costs them the rollout twice.
const inconclusiveGuidance = "nothing was written and no repository was opened — this is not a policy error; re-run when the lookup can be answered, or drop the owner check to write without it"

// verifyOwners runs R-41 over owners already known to be checkable, returning
// one error naming every owner that failed.
//
// Every owner is looked up even after the first bad answer, for R-22's
// reason: fixing a generated 40-op policy one refusal per invocation is
// miserable, and each invocation costs a fleet another round trip.
//
// A definitive "does not exist" outranks an inconclusive lookup when both
// occur. The dead owner is true whatever the rate limiter does next, so
// reporting it as something to re-run would send the operator back for the
// same refusal.
// verifier is what R-41 asks, which is one question more than lint asks. lint
// removes an owner GitHub does not know; R-41 refuses to WRITE one, so "this
// account exists" is not enough — a bare organization handle exists and still
// owns nobody.
type verifier interface {
	ownerid.Verifier
	AccountIsOrganization(login string) (bool, error)
}

func verifyOwners(v verifier, owners []string) error {
	var dead []string
	// Inconclusive lookups are grouped by REASON, not listed per owner. One
	// dead resolver or one rate limit makes every lookup in a 40-op baseline
	// fail the same way, and forty lines of the same sentence buries the one
	// fact the operator needs — which is the reason, not the roll call.
	var reasonOrder []string
	byReason := map[string][]string{}
	for _, o := range owners {
		gone, reason, lookupErr := ownerid.IsGone(v, o)
		// An account that exists is not yet an account GitHub will resolve as
		// an owner. `@acme` — the ORGANIZATION, not a team inside it — is a
		// valid owner token, answers 200 from /users/acme, and is ignored by
		// the CODEOWNERS resolver, which takes a user, an @org/team or an
		// email and nothing else. Proving existence and stopping there would
		// have written the reported bug's exact line through the check meant
		// to catch it. Asked only where it can be true: after existence, and
		// only for a bare handle.
		if lookupErr == nil && !gone {
			if _, _, isTeam := ownerid.SplitTeam(o); !isTeam {
				// Folded, exactly as ownerid.IsGone folds its own lookups
				// (R-38a): `@Acme` and `@acme` are one account, and asking
				// about the spelling the operator typed would both miss the
				// cache the existence check just filled and risk a 404 that
				// means nothing more than a capital letter.
				isOrg, orgErr := v.AccountIsOrganization(strings.ToLower(strings.TrimPrefix(o, "@")))
				switch {
				case orgErr != nil:
					lookupErr = orgErr
				case isOrg:
					gone = true
					reason = fmt.Sprintf("%s names an organization, not a user or a team; CODEOWNERS resolves an owner only as a user, an @org/team or an email address, so this one is nobody — you probably meant %s/<team>", o, o)
				}
			}
		}
		switch {
		case lookupErr != nil:
			r := ownerid.Reason(lookupErr)
			if _, seen := byReason[r]; !seen {
				reasonOrder = append(reasonOrder, r)
			}
			byReason[r] = append(byReason[r], o)
		case gone:
			// The owner's spelling AS WRITTEN leads, because that is the
			// string the operator has to find in the policy; ownerid folds
			// for the lookup and its reason carries the folded form.
			dead = append(dead, fmt.Sprintf(
				"%s cannot be written: %s — GitHub resolves an unknown owner to nobody and reports no error, so the rule would be written and own nothing (A-1, R-41)",
				o, reason))
		}
	}
	var inconclusive []string
	for _, r := range reasonOrder {
		inconclusive = append(inconclusive, fmt.Sprintf(
			"cannot verify %s (%s) — refusing to write an owner whose existence is unproven (R-12)",
			strings.Join(byReason[r], ", "), r))
	}
	switch {
	case len(dead) > 0:
		return &deadOwnersError{reasons: append(dead, inconclusive...), mixed: len(inconclusive) > 0}
	case len(inconclusive) > 0:
		return &inconclusiveOwnersError{reasons: inconclusive}
	}
	return nil
}

// verifyGuidance is the advice line for one of R-41's two refusals, and the
// reason exit3 takes a guidance parameter at all. Three cases, because two
// would have to lie about one of them:
//
//   - Every owner definitively dead: a policy error, and "do not retry" is
//     exactly right.
//   - Nothing definitive at all: the policy may be perfect and the token
//     merely rate-limited; sending that operator to edit a correct file costs
//     them the rollout twice.
//   - Both: "fix the policy" is true of the dead owners and false of the
//     lookups nobody could answer, and printing only the first sentence sends
//     the operator back for a second refusal after their fix lands.
func verifyGuidance(err error) string {
	var dead *deadOwnersError
	if errors.As(err, &dead) {
		if dead.mixed {
			return "fix the policy: the owners above that do not exist will fail identically on every repo. The lookups that could not be answered are a separate problem and that fix does not settle them — re-run once they can be answered"
		}
		return policyGuidance
	}
	return inconclusiveGuidance
}

// unverifiableNote discloses the owners R-41 wrote without checking. Silence
// would be the defect: the operator asked for verification and got a partial
// one, and "verified" is exactly what they would otherwise read into exit 0.
func unverifiableNote(owners []string) string {
	return fmt.Sprintf("note: %s written without verification — an email owner resolves through a verified address the API cannot see (R-13); confirm by hand",
		strings.Join(owners, ", "))
}

// verifyOwnersFor is the CLI-facing half of R-41: split the owners into the
// ones GitHub can answer for and the ones it cannot, resolve the credential,
// prove the API is reachable, then ask. Shared by `sync`, `check` and `plan`
// so the verbs cannot disagree about a policy — the property that makes
// `check` the cheap gate before repo #1.
func verifyOwnersFor(token, apiURL string, list []ops.Op) (unverifiable []string, err error) {
	var checkable []string
	for _, o := range ownersIntroduced(list) {
		// R-13: an email owner resolves through a verified address the API
		// cannot see. Unverifiable is not inconclusive — it is permanent, so
		// failing closed on it would make R-41 unusable for any policy that
		// has one, forever.
		if file.IsEmailOwner(o) {
			unverifiable = append(unverifiable, o)
			continue
		}
		checkable = append(checkable, o)
	}
	// Nothing to ask, so nothing is needed to ask it with. A wave whose ops
	// only REMOVE owners introduces nobody — and removing a team that was
	// deleted is the repair this whole area recommends, so refusing it for
	// want of a credential would refuse the cleanup R-41's own findings call
	// for. Same for `set_owners(/x/, [])`, and for a policy whose only owners
	// are email owners.
	if len(checkable) == 0 {
		return unverifiable, errNothingToVerify
	}
	// $GITHUB_TOKEN stays the documented fallback, resolved here rather than
	// at flag-parse time so nothing can render it; an explicit --token wins.
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}
	if token == "" {
		// R-12 in its bluntest form: with no credential the question has no
		// answer, and answering it anyway — by quietly doing the offline thing
		// — would report success for the very run the operator asked to be
		// checked. That vacuous pass is what let the reported wave ship.
		return unverifiable, &inconclusiveOwnersError{reasons: []string{
			"owner verification was asked for and there is no GitHub token: pass --token or set $GITHUB_TOKEN — owner existence is not decidable offline (R-12)"}}
	}
	// A per-run in-memory cache, deliberately not the disk cache audit offers:
	// a stale "this team exists" served from disk is the one answer that would
	// let R-41 wave through the write it exists to stop.
	client := ghapi.New(apiURL, token, ghapi.NewMemCache())
	// Before any owner is looked up, not after one 404s. A GHES --api-url
	// missing /api/v3 returns 404 from EVERY endpoint, so every owner in the
	// policy looks deleted and a hundred repositories are refused over one
	// typo in a URL — the mass false negative R-12 exists to prevent. ghapi
	// consults this on the user path only, where a 404 is otherwise
	// definitive; asking here means the TEAM path is diagnosed as a bad base
	// URL too, rather than as an org this token cannot enumerate.
	if err := client.ProbeAPI(); err != nil {
		return unverifiable, &inconclusiveOwnersError{reasons: []string{fmt.Sprintf(
			"cannot verify any owner (%s) — refusing to write owners whose existence is unproven (R-12)",
			ownerid.Reason(err))}}
	}
	return unverifiable, verifyOwners(client, checkable)
}

// errNothingToVerify is returned when the run introduces no owner the API can
// be asked about — a removal-only repair, an empty `set_owners`, or a policy
// whose only new owners are email owners. It is not a failure: the caller
// says so and carries on.
//
// Silence was the alternative and it is the shape R-41 exists to stop. The
// operator asked for verification, got exit 0, and no request was made; only
// a note distinguishes that from a wave whose owners were all checked.
var errNothingToVerify = errors.New("no owner in this run could be asked about")

// nothingToVerifyNote is that disclosure.
const nothingToVerifyNote = "note: owner verification was asked for and there was nothing to verify — this run introduces no owner the GitHub API can be asked about (a removal, an empty owner set, or email owners only; R-13)"

// idleCredentialNote names a credential that did nothing. `--token` and
// `--api-url` are only ever read by R-41, so passing them without asking for
// the check leaves an operator believing their owners were verified when no
// request was made — the same silent no-op shape as the bug R-41 exists to
// stop.
const idleCredentialNote = "note: --token/--api-url were given but no owner check was asked for, so no owner was verified — pass --verify-owners, or set \"verify_owners\": true in the policy"
