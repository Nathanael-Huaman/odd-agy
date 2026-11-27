package rdd

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Nathanael-Huaman/odd-agy/internal/config"
)

const (
	defaultCorrectionBudget = 1
	defaultScopeLimit       = 20
	maxEscalationReasonByte = 500
)

type persistedLineage struct {
	LineageStatus
	BaseRef            string `json:"baseRef,omitempty"`
	CommittedOnly      bool   `json:"committedOnly"`
	RequestContextHash string `json:"requestContextHash,omitempty"`
	ManifestFileSHA256 string `json:"manifestFileSha256"`
}

type consumedDigests struct {
	Digests map[string]string `json:"digests"` // candidateDigest -> lineageId
}

// Engine manages per-repository RDD mode, risk assessment, and frozen-snapshot review lineages.
type Engine struct {
	mu       sync.Mutex
	repoRoot string
}

// NewEngine creates an RDD Engine rooted at repoRoot.
func NewEngine(repoRoot string) *Engine {
	return &Engine{repoRoot: repoRoot}
}

// Mode reads or updates the project-scoped RDDEnabled flag in .agents/odd/config.json.
func (e *Engine) Mode(action string) (ModeResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	cfg, err := config.LoadConfig(e.repoRoot)
	if err != nil {
		return ModeResult{}, err
	}

	switch action {
	case "status":
	case "enable":
		cfg.RDDEnabled = true
		if err := config.SaveConfig(e.repoRoot, cfg); err != nil {
			return ModeResult{}, err
		}
	case "disable":
		cfg.RDDEnabled = false
		if err := config.SaveConfig(e.repoRoot, cfg); err != nil {
			return ModeResult{}, err
		}
	default:
		return ModeResult{}, fmt.Errorf("invalid mode action %q: must be status, enable, or disable", action)
	}

	return ModeResult{
		RDDEnabled: cfg.RDDEnabled,
		ConfigPath: config.ConfigPath(e.repoRoot),
	}, nil
}

// Assess evaluates candidate risk and review readiness without mutating RDD state.
func (e *Engine) Assess(req AssessRequest) (AssessResult, error) {
	if err := validateEscalation(req.Escalate); err != nil {
		return AssessResult{}, err
	}
	gitDir, err := resolveGitDir(e.repoRoot)
	if err != nil {
		return AssessResult{}, err
	}
	cfg, err := config.LoadConfig(e.repoRoot)
	if err != nil {
		return AssessResult{}, err
	}
	snap, err := collectCandidate(e.repoRoot, req.BaseRef, req.CommittedOnly)
	if err != nil {
		return AssessResult{}, err
	}

	nativeRisk, reason := classifyRisk(snap)
	effectiveRisk := nativeRisk
	independentVerifier := false
	if req.Escalate != nil {
		effectiveRisk = RiskHigh
		independentVerifier = true
	}

	alreadyApproved, err := e.isDigestConsumed(gitDir, snap.CandidateDigest)
	if err != nil {
		return AssessResult{}, err
	}

	var reviewDue bool
	var reviewDueReason string
	if !cfg.RDDEnabled {
		reviewDue = false
		reviewDueReason = "rdd is disabled for this project"
	} else if req.Escalate != nil {
		reviewDue = true
		reviewDueReason = fmt.Sprintf("escalated (item %d): %s", req.Escalate.Item, strings.TrimSpace(req.Escalate.Reason))
	} else if alreadyApproved {
		reviewDue = false
		reviewDueReason = "candidate digest already approved and acknowledged"
	} else if effectiveRisk == RiskMedium || effectiveRisk == RiskHigh {
		reviewDue = true
		reviewDueReason = reason
	} else {
		reviewDue = false
		reviewDueReason = reason
	}

	return AssessResult{
		RDDEnabled:          cfg.RDDEnabled,
		Risk:                effectiveRisk,
		NativeRisk:          nativeRisk,
		ReviewDue:           reviewDue,
		ReviewDueReason:     reviewDueReason,
		ChangedPaths:        snap.ChangedPaths,
		ChangedLines:        snap.ChangedLines,
		IndependentVerifier: independentVerifier,
		CandidateDigest:     snap.CandidateDigest,
		AlreadyApproved:     alreadyApproved,
	}, nil
}

// Start freezes the exact candidate snapshot and creates a lineage awaiting human consent.
func (e *Engine) Start(req StartRequest) (StartResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	cfg, err := config.LoadConfig(e.repoRoot)
	if err != nil {
		return StartResult{}, err
	}
	if !cfg.RDDEnabled {
		return StartResult{}, errors.New("rdd is disabled for this project; enable it before starting a review")
	}

	lenses, err := validateLenses(req.Lenses)
	if err != nil {
		return StartResult{}, err
	}
	lensesReason := strings.TrimSpace(req.LensesReason)
	if lensesReason == "" {
		return StartResult{}, errors.New("lensesReason must not be empty")
	}

	gitDir, err := resolveGitDir(e.repoRoot)
	if err != nil {
		return StartResult{}, err
	}
	snap, err := collectCandidate(e.repoRoot, req.BaseRef, req.CommittedOnly)
	if err != nil {
		return StartResult{}, err
	}

	var reqCtxBytes []byte
	var reqCtxHash string
	if strings.TrimSpace(req.RequestContextPath) != "" {
		reqCtxPath, err := resolveRequestContextPath(e.repoRoot, req.RequestContextPath)
		if err != nil {
			return StartResult{}, err
		}
		reqCtxBytes, err = os.ReadFile(reqCtxPath)
		if err != nil {
			return StartResult{}, fmt.Errorf("read requestContextPath %q: %w", req.RequestContextPath, err)
		}
		reqCtxHash = sha256Hex(reqCtxBytes)
	}

	lineageID, err := newLineageID()
	if err != nil {
		return StartResult{}, err
	}

	manifest, manifestFileHash, err := freezeSnapshot(gitDir, lineageID, 1, req.BaseRef, req.CommittedOnly, lenses, lensesReason, reqCtxHash, reqCtxBytes, snap)
	if err != nil {
		return StartResult{}, err
	}

	now := nowUTC()
	st := persistedLineage{
		LineageStatus: LineageStatus{
			LineageID:        lineageID,
			Status:           StatusConsentRequired,
			Revision:         1,
			ManifestSHA256:   manifest.ManifestSHA256,
			CandidateDigest:  snap.CandidateDigest,
			Lenses:           append([]string(nil), lenses...),
			LensesReason:     lensesReason,
			PendingLenses:    append([]string(nil), lenses...),
			CompletedLenses:  []string{},
			Advisories:       []Finding{},
			Blockers:         []Finding{},
			CorrectionBudget: defaultCorrectionBudget,
			CorrectionsUsed:  0,
			Authority:        AuthorityPending,
			NextAction:       "answer_consent",
			CreatedAt:        now,
			UpdatedAt:        now,
		},
		BaseRef:            req.BaseRef,
		CommittedOnly:      req.CommittedOnly,
		RequestContextHash: reqCtxHash,
		ManifestFileSHA256: manifestFileHash,
	}

	if err := saveLineage(gitDir, st); err != nil {
		return StartResult{}, err
	}

	return StartResult{
		LineageID:       lineageID,
		ManifestSHA256:  manifest.ManifestSHA256,
		CandidateDigest: snap.CandidateDigest,
		ConsentRequired: true,
		ConsentPrompt:   fmt.Sprintf("Review lineage %s frozen (%d files, sha256=%s). Answer consent with \"granted\" or \"declined\".", lineageID, len(manifest.Files), manifest.ManifestSHA256),
		NextAction:      "answer_consent",
	}, nil
}

// AnswerConsent records the human decision ("granted" or "declined") on a frozen lineage.
func (e *Engine) AnswerConsent(lineageID, answer string) (LineageStatus, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if answer != "granted" && answer != "declined" {
		return LineageStatus{}, fmt.Errorf("invalid consent answer %q: must be \"granted\" or \"declined\"", answer)
	}

	gitDir, err := resolveGitDir(e.repoRoot)
	if err != nil {
		return LineageStatus{}, err
	}
	st, err := loadLineage(gitDir, lineageID)
	if err != nil {
		return LineageStatus{}, err
	}
	if st.Status != StatusConsentRequired {
		return LineageStatus{}, fmt.Errorf("lineage %s is in status %q, expected %q", lineageID, st.Status, StatusConsentRequired)
	}

	if answer == "declined" {
		st.Status = StatusDeclined
		st.NextAction = "none"
	} else {
		st.Status = StatusCollectingLenses
		st.PendingLenses = append([]string(nil), st.Lenses...)
		st.CompletedLenses = []string{}
		st.NextAction = "submit_lenses"
	}
	st.UpdatedAt = nowUTC()

	if err := saveLineage(gitDir, st); err != nil {
		return LineageStatus{}, err
	}
	return cloneStatus(st.LineageStatus), nil
}

// Scope returns a paginated, SHA-256-verified read-only view of the frozen snapshot.
// It never reads the live working tree and fails closed on any tamper detection.
func (e *Engine) Scope(lineageID string, cursor, limit int) (ScopePage, error) {
	if cursor < 0 {
		return ScopePage{}, fmt.Errorf("invalid negative cursor %d", cursor)
	}
	if limit <= 0 {
		limit = defaultScopeLimit
	}

	gitDir, err := resolveGitDir(e.repoRoot)
	if err != nil {
		return ScopePage{}, err
	}
	st, err := loadLineage(gitDir, lineageID)
	if err != nil {
		return ScopePage{}, err
	}

	manifest, err := loadAndVerifyManifest(gitDir, st)
	if err != nil {
		return ScopePage{}, err
	}

	total := len(manifest.Files)
	start := cursor
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}

	files := append([]FrozenFile(nil), manifest.Files[start:end]...)
	hasMore := end < total
	nextCursor := 0
	if hasMore {
		nextCursor = end
	}

	return ScopePage{
		LineageID:      lineageID,
		Revision:       manifest.Revision,
		ManifestSHA256: manifest.ManifestSHA256,
		TotalFiles:     total,
		Cursor:         cursor,
		Limit:          limit,
		NextCursor:     nextCursor,
		HasMore:        hasMore,
		Files:          files,
	}, nil
}

// Submit admits structured findings for one pending 4R lens or verdicts for the refuter pass.
func (e *Engine) Submit(req SubmitRequest) (LineageStatus, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	gitDir, err := resolveGitDir(e.repoRoot)
	if err != nil {
		return LineageStatus{}, err
	}
	st, err := loadLineage(gitDir, req.LineageID)
	if err != nil {
		return LineageStatus{}, err
	}

	if req.Lens == LensRefuter {
		if st.Status != StatusCollectingRefuter {
			return LineageStatus{}, fmt.Errorf("refuter verdicts cannot be submitted while status is %q", st.Status)
		}
		if len(req.Findings) > 0 {
			return LineageStatus{}, errors.New("refuter submission must provide verdicts, not findings")
		}
		if err := applyRefuterVerdicts(&st, req.Verdicts); err != nil {
			return LineageStatus{}, err
		}
		st.UpdatedAt = nowUTC()
		if err := saveLineage(gitDir, st); err != nil {
			return LineageStatus{}, err
		}
		return cloneStatus(st.LineageStatus), nil
	}

	if !isValid4RLens(req.Lens) {
		return LineageStatus{}, fmt.Errorf("unknown lens %q", req.Lens)
	}
	if st.Status != StatusCollectingLenses {
		return LineageStatus{}, fmt.Errorf("lens findings cannot be submitted while status is %q", st.Status)
	}
	if len(req.Verdicts) > 0 {
		return LineageStatus{}, errors.New("lens submission must provide findings, not refuter verdicts")
	}
	if !slices.Contains(st.PendingLenses, req.Lens) {
		return LineageStatus{}, fmt.Errorf("lens %q is not pending for lineage %s", req.Lens, req.LineageID)
	}

	existingIDs := make(map[string]bool, len(st.Advisories)+len(st.Blockers))
	for _, f := range st.Advisories {
		existingIDs[f.ID] = true
	}
	for _, f := range st.Blockers {
		existingIDs[f.ID] = true
	}

	newAdvisories := make([]Finding, 0)
	newBlockers := make([]Finding, 0)
	for _, f := range req.Findings {
		if err := validateFinding(f); err != nil {
			return LineageStatus{}, err
		}
		if existingIDs[f.ID] {
			return LineageStatus{}, fmt.Errorf("duplicate finding id %q", f.ID)
		}
		existingIDs[f.ID] = true
		f.Lens = req.Lens
		if f.Severity == SeveritySEVERE && (f.Causality == CausalityCandidate || f.Causality == CausalityUnknown) {
			newBlockers = append(newBlockers, f)
		} else {
			newAdvisories = append(newAdvisories, f)
		}
	}

	st.Advisories = append(st.Advisories, newAdvisories...)
	st.Blockers = append(st.Blockers, newBlockers...)
	st.PendingLenses = removeString(st.PendingLenses, req.Lens)
	st.CompletedLenses = append(st.CompletedLenses, req.Lens)

	if len(st.PendingLenses) == 0 {
		if len(st.Blockers) == 0 {
			st.Status = StatusApprovedPending
			st.Authority = AuthorityActive
			st.NextAction = "ack"
		} else {
			st.Status = StatusCollectingRefuter
			st.NextAction = "submit_refuter"
		}
	}
	st.UpdatedAt = nowUTC()

	if err := saveLineage(gitDir, st); err != nil {
		return LineageStatus{}, err
	}
	return cloneStatus(st.LineageStatus), nil
}

// ApplyCorrection re-freezes the updated candidate as revision 2 when in correction_required
// and within the 1-correction budget.
func (e *Engine) ApplyCorrection(lineageID string) (LineageStatus, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	gitDir, err := resolveGitDir(e.repoRoot)
	if err != nil {
		return LineageStatus{}, err
	}
	st, err := loadLineage(gitDir, lineageID)
	if err != nil {
		return LineageStatus{}, err
	}
	if st.Status != StatusCorrectionReq {
		return LineageStatus{}, fmt.Errorf("ApplyCorrection requires status %q, got %q", StatusCorrectionReq, st.Status)
	}
	if st.CorrectionsUsed >= st.CorrectionBudget {
		return LineageStatus{}, fmt.Errorf("correction budget exhausted (%d/%d)", st.CorrectionsUsed, st.CorrectionBudget)
	}

	snap, err := collectCandidate(e.repoRoot, st.BaseRef, st.CommittedOnly)
	if err != nil {
		return LineageStatus{}, err
	}

	nextRev := st.Revision + 1
	var reqCtxBytes []byte
	if st.RequestContextHash != "" {
		// Copy preserved request context from revision 1 if present.
		prevCtxPath := filepath.Join(candidateDir(gitDir, lineageID), fmt.Sprintf("rev-%d", st.Revision), "request-context.txt")
		reqCtxBytes, _ = os.ReadFile(prevCtxPath)
	}

	manifest, manifestFileHash, err := freezeSnapshot(gitDir, lineageID, nextRev, st.BaseRef, st.CommittedOnly, st.Lenses, st.LensesReason, st.RequestContextHash, reqCtxBytes, snap)
	if err != nil {
		return LineageStatus{}, err
	}

	st.Revision = nextRev
	st.CorrectionsUsed++
	st.ManifestSHA256 = manifest.ManifestSHA256
	st.ManifestFileSHA256 = manifestFileHash
	st.CandidateDigest = snap.CandidateDigest
	st.Status = StatusCollectingLenses
	st.PendingLenses = append([]string(nil), st.Lenses...)
	st.CompletedLenses = []string{}
	st.Blockers = []Finding{}
	st.RefuterVerdicts = nil
	st.NextAction = "submit_lenses"
	st.UpdatedAt = nowUTC()

	if err := saveLineage(gitDir, st); err != nil {
		return LineageStatus{}, err
	}
	return cloneStatus(st.LineageStatus), nil
}

// Status returns the full deterministic state of an RDD lineage.
func (e *Engine) Status(lineageID string) (LineageStatus, error) {
	gitDir, err := resolveGitDir(e.repoRoot)
	if err != nil {
		return LineageStatus{}, err
	}
	st, err := loadLineage(gitDir, lineageID)
	if err != nil {
		return LineageStatus{}, err
	}
	return cloneStatus(st.LineageStatus), nil
}

// Ack burns approval authority for an approved_pending_ack lineage and marks its
// CandidateDigest as consumed for subsequent Assess calls.
func (e *Engine) Ack(lineageID string) (AckResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	gitDir, err := resolveGitDir(e.repoRoot)
	if err != nil {
		return AckResult{}, err
	}
	st, err := loadLineage(gitDir, lineageID)
	if err != nil {
		return AckResult{}, err
	}
	if st.Status != StatusApprovedPending {
		return AckResult{}, fmt.Errorf("Ack requires status %q, got %q", StatusApprovedPending, st.Status)
	}

	if err := e.recordConsumedDigest(gitDir, st.CandidateDigest, lineageID); err != nil {
		return AckResult{}, err
	}

	st.Status = StatusClosed
	st.Authority = AuthorityBurned
	st.DeliveryPolicy = DeliveryPolicyOrdinary
	st.NextAction = "none"
	st.UpdatedAt = nowUTC()

	if err := saveLineage(gitDir, st); err != nil {
		return AckResult{}, err
	}

	return AckResult{
		LineageID:       lineageID,
		Status:          StatusClosed,
		Authority:       AuthorityBurned,
		CandidateDigest: st.CandidateDigest,
		DeliveryPolicy:  DeliveryPolicyOrdinary,
	}, nil
}

func validateEscalation(esc *Escalation) error {
	if esc == nil {
		return nil
	}
	if esc.Item < 1 || esc.Item > 6 {
		return fmt.Errorf("escalation item %d must be strictly between 1 and 6", esc.Item)
	}
	if strings.TrimSpace(esc.Reason) == "" {
		return errors.New("escalation reason must not be empty")
	}
	if len(esc.Reason) > maxEscalationReasonByte {
		return fmt.Errorf("escalation reason exceeds %d UTF-8 bytes (%d)", maxEscalationReasonByte, len(esc.Reason))
	}
	return nil
}

func validateLenses(in []string) ([]string, error) {
	if len(in) < 1 || len(in) > 4 {
		return nil, fmt.Errorf("lenses must contain 1 to 4 unique items, got %d", len(in))
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, l := range in {
		if !isValid4RLens(l) {
			return nil, fmt.Errorf("invalid lens %q: must be one of risk, resilience, readability, reliability", l)
		}
		if seen[l] {
			return nil, fmt.Errorf("duplicate lens %q", l)
		}
		seen[l] = true
		out = append(out, l)
	}
	return out, nil
}

func isValid4RLens(l string) bool {
	switch l {
	case LensRisk, LensResilience, LensReadability, LensReliability:
		return true
	default:
		return false
	}
}

func validateFinding(f Finding) error {
	if strings.TrimSpace(f.ID) == "" {
		return errors.New("finding id must not be empty")
	}
	if strings.TrimSpace(f.Location) == "" {
		return fmt.Errorf("finding %q location must not be empty", f.ID)
	}
	if strings.TrimSpace(f.Claim) == "" {
		return fmt.Errorf("finding %q claim must not be empty", f.ID)
	}
	switch f.Severity {
	case SeveritySEVERE, SeverityWARNING, SeveritySUGGESTION:
	default:
		return fmt.Errorf("finding %q invalid severity %q", f.ID, f.Severity)
	}
	switch f.Causality {
	case CausalityCandidate, CausalityPreExisting, CausalityUnknown:
	default:
		return fmt.Errorf("finding %q invalid causality %q", f.ID, f.Causality)
	}
	return nil
}

func applyRefuterVerdicts(st *persistedLineage, verdicts []RefuterVerdict) error {
	verdictMap := make(map[string]RefuterVerdict, len(verdicts))
	for _, v := range verdicts {
		if strings.TrimSpace(v.FindingID) == "" {
			return errors.New("refuter verdict findingId must not be empty")
		}
		switch v.Verdict {
		case VerdictConfirmed, VerdictRefuted, VerdictUnknown:
		default:
			return fmt.Errorf("invalid refuter verdict %q for %q", v.Verdict, v.FindingID)
		}
		if _, dup := verdictMap[v.FindingID]; dup {
			return fmt.Errorf("duplicate refuter verdict for finding %q", v.FindingID)
		}
		verdictMap[v.FindingID] = v
	}

	blockerIDs := make(map[string]bool, len(st.Blockers))
	remainingBlockers := make([]Finding, 0, len(st.Blockers))
	hasUnknown := false
	hasConfirmed := false

	for _, b := range st.Blockers {
		blockerIDs[b.ID] = true
		v, ok := verdictMap[b.ID]
		if !ok {
			return fmt.Errorf("missing refuter verdict for blocker %q", b.ID)
		}
		switch v.Verdict {
		case VerdictRefuted:
			st.Advisories = append(st.Advisories, b)
		case VerdictUnknown:
			hasUnknown = true
			remainingBlockers = append(remainingBlockers, b)
		case VerdictConfirmed:
			hasConfirmed = true
			remainingBlockers = append(remainingBlockers, b)
		}
	}

	for id := range verdictMap {
		if !blockerIDs[id] {
			return fmt.Errorf("refuter verdict references unknown blocker %q", id)
		}
	}

	st.Blockers = remainingBlockers
	st.RefuterVerdicts = append([]RefuterVerdict(nil), verdicts...)

	if len(remainingBlockers) == 0 {
		st.Status = StatusApprovedPending
		st.Authority = AuthorityActive
		st.NextAction = "ack"
		return nil
	}
	if hasUnknown {
		st.Status = StatusEscalated
		st.NextAction = "human_escalation"
		return nil
	}
	if hasConfirmed {
		if st.CorrectionsUsed < st.CorrectionBudget {
			st.Status = StatusCorrectionReq
			st.NextAction = "apply_correction"
		} else {
			st.Status = StatusEscalated
			st.NextAction = "human_escalation"
		}
	}
	return nil
}

func freezeSnapshot(
	gitDir, lineageID string,
	revision int,
	baseRef string,
	committedOnly bool,
	lenses []string,
	lensesReason string,
	reqCtxHash string,
	reqCtxBytes []byte,
	snap candidateSnapshot,
) (Manifest, string, error) {
	candDir := candidateDir(gitDir, lineageID)
	revDir := filepath.Join(candDir, fmt.Sprintf("rev-%d", revision))
	filesDir := filepath.Join(revDir, "files")

	// Ensure candidate dir is writable when adding revision 2+.
	if info, err := os.Stat(candDir); err == nil && info.IsDir() {
		if err := os.Chmod(candDir, 0o700); err != nil {
			return Manifest{}, "", fmt.Errorf("unlock candidate dir %s: %w", candDir, err)
		}
	}
	manifestPath := filepath.Join(candDir, "manifest.json")
	if _, err := os.Stat(manifestPath); err == nil {
		if err := os.Chmod(manifestPath, 0o600); err != nil {
			return Manifest{}, "", fmt.Errorf("unlock manifest %s: %w", manifestPath, err)
		}
	}

	if err := os.MkdirAll(filesDir, 0o700); err != nil {
		return Manifest{}, "", fmt.Errorf("create snapshot dir %s: %w", filesDir, err)
	}

	createdDirs := map[string]bool{candDir: true, revDir: true, filesDir: true}
	var createdFiles []string

	if reqCtxHash != "" {
		ctxPath := filepath.Join(revDir, "request-context.txt")
		if err := os.WriteFile(ctxPath, reqCtxBytes, 0o400); err != nil {
			return Manifest{}, "", fmt.Errorf("write frozen request context: %w", err)
		}
		createdFiles = append(createdFiles, ctxPath)
	}

	for _, f := range snap.Files {
		if f.Status == "D" {
			continue
		}
		dst := filepath.Join(filesDir, filepath.FromSlash(f.Path))
		parent := filepath.Dir(dst)
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return Manifest{}, "", fmt.Errorf("create snapshot parent %s: %w", parent, err)
		}
		for cur := parent; strings.HasPrefix(cur, filesDir); cur = filepath.Dir(cur) {
			createdDirs[cur] = true
			if cur == filesDir {
				break
			}
		}
		if err := os.WriteFile(dst, []byte(f.Content), 0o400); err != nil {
			return Manifest{}, "", fmt.Errorf("write frozen file %s: %w", dst, err)
		}
		createdFiles = append(createdFiles, dst)
	}

	manifest := Manifest{
		LineageID:          lineageID,
		Revision:           revision,
		BaseRef:            baseRef,
		CommittedOnly:      committedOnly,
		Lenses:             append([]string(nil), lenses...),
		LensesReason:       lensesReason,
		RequestContextHash: reqCtxHash,
		CandidateDigest:    snap.CandidateDigest,
		Files:              append([]FrozenFile(nil), snap.Files...),
	}

	canonicalBytes, err := json.Marshal(manifest)
	if err != nil {
		return Manifest{}, "", fmt.Errorf("marshal canonical manifest: %w", err)
	}
	manifest.ManifestSHA256 = sha256Hex(canonicalBytes)

	finalBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, "", fmt.Errorf("marshal manifest.json: %w", err)
	}
	finalBytes = append(finalBytes, '\n')
	manifestFileHash := sha256Hex(finalBytes)

	if err := os.WriteFile(manifestPath, finalBytes, 0o600); err != nil {
		return Manifest{}, "", fmt.Errorf("write manifest.json: %w", err)
	}
	if err := os.Chmod(manifestPath, 0o400); err != nil {
		return Manifest{}, "", fmt.Errorf("chmod manifest.json: %w", err)
	}
	for _, fp := range createdFiles {
		_ = os.Chmod(fp, 0o400)
	}

	// Lock directories deepest-first so files inside cannot be added/removed/renamed.
	dirs := make([]string, 0, len(createdDirs))
	for d := range createdDirs {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool {
		return len(dirs[i]) > len(dirs[j])
	})
	for _, d := range dirs {
		if err := os.Chmod(d, 0o500); err != nil {
			return Manifest{}, "", fmt.Errorf("chmod snapshot dir %s: %w", d, err)
		}
	}

	return manifest, manifestFileHash, nil
}

func resolveRequestContextPath(repoRoot, raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if filepath.IsAbs(trimmed) {
		return "", fmt.Errorf("requestContextPath %q must be a relative path inside the repository", raw)
	}
	realRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", fmt.Errorf("resolve repository root %q: %w", repoRoot, err)
	}
	realTarget, err := filepath.EvalSymlinks(filepath.Join(realRoot, trimmed))
	if err != nil {
		return "", fmt.Errorf("resolve requestContextPath %q: %w", raw, err)
	}
	rel, err := filepath.Rel(realRoot, realTarget)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("requestContextPath %q escapes repository root", raw)
	}
	info, err := os.Stat(realTarget)
	if err != nil {
		return "", fmt.Errorf("stat requestContextPath %q: %w", raw, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("requestContextPath %q must be a regular file", raw)
	}
	return realTarget, nil
}

func loadAndVerifyManifest(gitDir string, st persistedLineage) (Manifest, error) {
	candDir := candidateDir(gitDir, st.LineageID)
	manifestPath := filepath.Join(candDir, "manifest.json")
	rawBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return Manifest{}, fmt.Errorf("read frozen manifest %s: %w", manifestPath, err)
	}

	if actualFileHash := sha256Hex(rawBytes); actualFileHash != st.ManifestFileSHA256 {
		return Manifest{}, fmt.Errorf("tamper detected: manifest.json sha256 mismatch (got %s, want %s)", actualFileHash, st.ManifestFileSHA256)
	}

	var manifest Manifest
	if err := json.Unmarshal(rawBytes, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("tamper detected: corrupt manifest.json: %w", err)
	}

	storedHash := manifest.ManifestSHA256
	manifest.ManifestSHA256 = ""
	canonicalBytes, err := json.Marshal(manifest)
	if err != nil {
		return Manifest{}, fmt.Errorf("marshal manifest for verification: %w", err)
	}
	recomputed := sha256Hex(canonicalBytes)
	if recomputed != storedHash || recomputed != st.ManifestSHA256 {
		return Manifest{}, fmt.Errorf("tamper detected: canonical manifest sha256 mismatch (got %s, want %s)", recomputed, st.ManifestSHA256)
	}
	manifest.ManifestSHA256 = storedHash

	filesDir := filepath.Join(candDir, fmt.Sprintf("rev-%d", manifest.Revision), "files")
	for _, f := range manifest.Files {
		if f.Status == "D" {
			continue
		}
		frozenPath := filepath.Join(filesDir, filepath.FromSlash(f.Path))
		contentBytes, err := os.ReadFile(frozenPath)
		if err != nil {
			return Manifest{}, fmt.Errorf("tamper detected: missing frozen file %s: %w", f.Path, err)
		}
		if sha256Hex(contentBytes) != f.ContentSHA256 {
			return Manifest{}, fmt.Errorf("tamper detected: frozen file %s sha256 mismatch", f.Path)
		}
	}

	return manifest, nil
}

func candidateDir(gitDir, lineageID string) string {
	return filepath.Join(gitDir, "odd-agy", "candidates", lineageID)
}

func lineagePath(gitDir, lineageID string) string {
	return filepath.Join(gitDir, "odd-agy", "rdd", "lineages", lineageID+".json")
}

func consumedPath(gitDir string) string {
	return filepath.Join(gitDir, "odd-agy", "rdd", "consumed-digests.json")
}

func loadLineage(gitDir, lineageID string) (persistedLineage, error) {
	if strings.TrimSpace(lineageID) == "" || strings.Contains(lineageID, "..") || strings.ContainsAny(lineageID, "/\\") {
		return persistedLineage{}, fmt.Errorf("invalid lineageId %q", lineageID)
	}
	data, err := os.ReadFile(lineagePath(gitDir, lineageID))
	if err != nil {
		return persistedLineage{}, fmt.Errorf("load lineage %s: %w", lineageID, err)
	}
	var st persistedLineage
	if err := json.Unmarshal(data, &st); err != nil {
		return persistedLineage{}, fmt.Errorf("parse lineage %s: %w", lineageID, err)
	}
	return st, nil
}

func saveLineage(gitDir string, st persistedLineage) error {
	return writeJSONAtomic(lineagePath(gitDir, st.LineageID), st)
}

func (e *Engine) isDigestConsumed(gitDir, digest string) (bool, error) {
	data, err := os.ReadFile(consumedPath(gitDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read consumed digests: %w", err)
	}
	var c consumedDigests
	if err := json.Unmarshal(data, &c); err != nil {
		return false, fmt.Errorf("parse consumed digests: %w", err)
	}
	_, ok := c.Digests[digest]
	return ok, nil
}

func (e *Engine) recordConsumedDigest(gitDir, digest, lineageID string) error {
	var c consumedDigests
	data, err := os.ReadFile(consumedPath(gitDir))
	if err == nil {
		if err := json.Unmarshal(data, &c); err != nil {
			return fmt.Errorf("parse consumed digests: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read consumed digests: %w", err)
	}
	if c.Digests == nil {
		c.Digests = make(map[string]string)
	}
	c.Digests[digest] = lineageID
	return writeJSONAtomic(consumedPath(gitDir), c)
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json for %s: %w", path, err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*.json")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpName, path, err)
	}
	cleanup = false
	return nil
}

func newLineageID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate lineage id: %w", err)
	}
	return "review-" + hex.EncodeToString(buf[:]), nil
}

func removeString(items []string, target string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item != target {
			out = append(out, item)
		}
	}
	return out
}

func cloneStatus(st LineageStatus) LineageStatus {
	out := st
	out.Lenses = append([]string(nil), st.Lenses...)
	out.PendingLenses = append([]string(nil), st.PendingLenses...)
	out.CompletedLenses = append([]string(nil), st.CompletedLenses...)
	out.Advisories = append([]Finding(nil), st.Advisories...)
	out.Blockers = append([]Finding(nil), st.Blockers...)
	if st.RefuterVerdicts != nil {
		out.RefuterVerdicts = append([]RefuterVerdict(nil), st.RefuterVerdicts...)
	}
	return out
}

func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}
