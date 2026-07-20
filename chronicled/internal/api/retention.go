package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/zkrebbekx/chronicle/retain"
)

// sweepRequest is the body of POST /v1/retention/sweep.
type sweepRequest struct {
	Policies []policyRequest `json:"policies"`
	// DryRun runs retain.Plan instead of retain.Execute: the same decision
	// logic, nothing archived, nothing destroyed. Run it first.
	DryRun bool `json:"dryRun"`
}

type policyRequest struct {
	Kind string `json:"kind"`
	// KeepFor is a Go duration string ("8760h" is one year). Retention
	// eligibility is measured from the instant a record was superseded
	// (TxTo), never from when it was written, and a current record is never
	// eligible at any age.
	KeepFor string `json:"keepFor"`
}

// handleSweep is POST /v1/retention/sweep (admin). An empty policy list is
// refused — the library's ErrNoPolicy, surfaced as 400 no_policy — because
// there is no default retention period on purpose: what to destroy and when
// is a regulatory decision the service cannot make. See docs/COMPLIANCE.md.
func (s *Server) handleSweep(w http.ResponseWriter, r *http.Request) {
	var req sweepRequest
	if err := decodeBody(w, r, &req); err != nil {
		s.respondError(w, r, err)
		return
	}

	policies := make([]retain.Policy, 0, len(req.Policies))
	for i, p := range req.Policies {
		keepFor, err := time.ParseDuration(p.KeepFor)
		if err != nil {
			s.respondError(w, r, badRequest("invalid_policy",
				fmt.Sprintf("policies[%d].keepFor must be a Go duration such as \"8760h\", got %q", i, p.KeepFor)))
			return
		}
		policies = append(policies, retain.Policy{Kind: p.Kind, KeepFor: keepFor})
	}

	// The cutoff clock is this process's, compared against transaction
	// instants the store assigned. Retention periods dwarf clock skew in
	// practice; see retain.Execute's note.
	now := time.Now().UTC()
	var (
		report retain.Report
		err    error
	)
	if req.DryRun {
		report, err = retain.Plan(r.Context(), s.store, policies, now)
	} else {
		report, err = retain.Execute(r.Context(), s.store, policies, now)
	}
	if err != nil {
		s.respondError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toReportDTO(report))
}
