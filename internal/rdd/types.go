package rdd

const (
	RiskPassive = "passive"
	RiskMedium  = "medium"
	RiskHigh    = "high"

	LensRisk        = "risk"
	LensResilience  = "resilience"
	LensReadability = "readability"
	LensReliability = "reliability"
	LensRefuter     = "refuter"

	StatusConsentRequired   = "consent_required"
	StatusDeclined          = "declined"
	StatusCollectingLenses  = "collecting_lenses"
	StatusCollectingRefuter = "collecting_refuter"
	StatusCorrectionReq     = "correction_required"
	StatusApprovedPending   = "approved_pending_ack"
	StatusEscalated         = "escalated"
	StatusClosed            = "closed"

	SeveritySEVERE     = "SEVERE"
	SeverityWARNING    = "WARNING"
	SeveritySUGGESTION = "SUGGESTION"

	CausalityCandidate   = "candidate"
	CausalityPreExisting = "pre_existing"
	CausalityUnknown     = "unknown"

	VerdictConfirmed = "confirmed"
	VerdictRefuted   = "refuted"
	VerdictUnknown   = "unknown"

	AuthorityPending = "pending"
	AuthorityActive  = "approved"
	AuthorityBurned  = "burned"

	DeliveryPolicyOrdinary = "ordinary-repository-policy"
)

// ModeResult reports the current project RDD toggle state.
type ModeResult struct {
	RDDEnabled bool   `json:"rddEnabled"`
	ConfigPath string `json:"configPath"`
}

// Escalation represents an explicit Risk item 1..6 escalation with rationale.
type Escalation struct {
	Item   int    `json:"item"`
	Reason string `json:"reason"`
}

// AssessRequest specifies parameters for read-only risk and review-due assessment.
type AssessRequest struct {
	BaseRef       string      `json:"baseRef,omitempty"`
	CommittedOnly bool        `json:"committedOnly,omitempty"`
	Escalate      *Escalation `json:"escalate,omitempty"`
}

// AssessResult describes risk tier, review readiness, and candidate digest.
type AssessResult struct {
	RDDEnabled          bool     `json:"rddEnabled"`
	Risk                string   `json:"risk"`
	NativeRisk          string   `json:"nativeRisk"`
	ReviewDue           bool     `json:"reviewDue"`
	ReviewDueReason     string   `json:"reviewDueReason,omitempty"`
	ChangedPaths        []string `json:"changedPaths"`
	ChangedLines        int      `json:"changedLines"`
	IndependentVerifier bool     `json:"independentVerifier"`
	CandidateDigest     string   `json:"candidateDigest"`
	AlreadyApproved     bool     `json:"alreadyApproved"`
}

// StartRequest initiates a frozen-snapshot review lineage.
type StartRequest struct {
	BaseRef            string   `json:"baseRef,omitempty"`
	CommittedOnly      bool     `json:"committedOnly,omitempty"`
	Lenses             []string `json:"lenses"`
	LensesReason       string   `json:"lensesReason"`
	RequestContextPath string   `json:"requestContextPath,omitempty"`
}

// StartResult returns the created lineage and human consent gate details.
type StartResult struct {
	LineageID       string `json:"lineageId"`
	ManifestSHA256  string `json:"manifestSha256"`
	CandidateDigest string `json:"candidateDigest"`
	ConsentRequired bool   `json:"consentRequired"`
	ConsentPrompt   string `json:"consentPrompt"`
	NextAction      string `json:"nextAction"`
}

// FrozenFile represents one candidate file entry stored inside a frozen manifest.
type FrozenFile struct {
	Path          string `json:"path"`
	Status        string `json:"status"`
	OldMode       string `json:"oldMode,omitempty"`
	NewMode       string `json:"newMode,omitempty"`
	Diff          string `json:"diff"`
	Content       string `json:"content,omitempty"`
	ContentSHA256 string `json:"contentSha256,omitempty"`
	ChangedLines  int    `json:"changedLines"`
}

// Manifest is the canonical immutable candidate snapshot saved at manifest.json.
type Manifest struct {
	LineageID          string       `json:"lineageId"`
	Revision           int          `json:"revision"`
	BaseRef            string       `json:"baseRef,omitempty"`
	CommittedOnly      bool         `json:"committedOnly"`
	Lenses             []string     `json:"lenses"`
	LensesReason       string       `json:"lensesReason"`
	RequestContextHash string       `json:"requestContextHash,omitempty"`
	CandidateDigest    string       `json:"candidateDigest"`
	Files              []FrozenFile `json:"files"`
	ManifestSHA256     string       `json:"manifestSha256"`
}

// ScopePage returns a bounded paginated view of the frozen snapshot.
type ScopePage struct {
	LineageID      string       `json:"lineageId"`
	Revision       int          `json:"revision"`
	ManifestSHA256 string       `json:"manifestSha256"`
	TotalFiles     int          `json:"totalFiles"`
	Cursor         int          `json:"cursor"`
	Limit          int          `json:"limit"`
	NextCursor     int          `json:"nextCursor"`
	HasMore        bool         `json:"hasMore"`
	Files          []FrozenFile `json:"files"`
}

// Finding is a structured review finding emitted by a 4R lens.
type Finding struct {
	ID        string `json:"id"`
	Severity  string `json:"severity"`
	Causality string `json:"causality"`
	Location  string `json:"location"`
	Claim     string `json:"claim"`
	Lens      string `json:"lens,omitempty"`
}

// RefuterVerdict records the refuter's decision on a single severe blocker.
type RefuterVerdict struct {
	FindingID string `json:"findingId"`
	Verdict   string `json:"verdict"`
	Rationale string `json:"rationale,omitempty"`
}

// SubmitRequest submits either one 4R lens's findings or the refuter's verdicts.
type SubmitRequest struct {
	LineageID string           `json:"lineageId"`
	Lens      string           `json:"lens"`
	Findings  []Finding        `json:"findings,omitempty"`
	Verdicts  []RefuterVerdict `json:"verdicts,omitempty"`
}

// LineageStatus represents the full deterministic state of an RDD lineage.
type LineageStatus struct {
	LineageID        string           `json:"lineageId"`
	Status           string           `json:"status"`
	Revision         int              `json:"revision"`
	ManifestSHA256   string           `json:"manifestSha256"`
	CandidateDigest  string           `json:"candidateDigest"`
	Lenses           []string         `json:"lenses"`
	LensesReason     string           `json:"lensesReason"`
	PendingLenses    []string         `json:"pendingLenses"`
	CompletedLenses  []string         `json:"completedLenses"`
	Advisories       []Finding        `json:"advisories"`
	Blockers         []Finding        `json:"blockers"`
	RefuterVerdicts  []RefuterVerdict `json:"refuterVerdicts,omitempty"`
	CorrectionBudget int              `json:"correctionBudget"`
	CorrectionsUsed  int              `json:"correctionsUsed"`
	Authority        string           `json:"authority"`
	NextAction       string           `json:"nextAction"`
	DeliveryPolicy   string           `json:"deliveryPolicy,omitempty"`
	CreatedAt        string           `json:"createdAt"`
	UpdatedAt        string           `json:"updatedAt"`
}

// AckResult records the burned approval authority for a closed lineage.
type AckResult struct {
	LineageID       string `json:"lineageId"`
	Status          string `json:"status"`
	Authority       string `json:"authority"`
	CandidateDigest string `json:"candidateDigest"`
	DeliveryPolicy  string `json:"deliveryPolicy"`
}
