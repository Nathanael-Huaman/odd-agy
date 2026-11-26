package rdd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nathanael-Huaman/odd-agy/internal/config"
)

func initTempRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test User")
	t.Cleanup(func() {
		candRoot := filepath.Join(dir, ".git", "odd-agy", "candidates")
		_ = filepath.WalkDir(candRoot, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				_ = os.Chmod(path, 0o755)
			} else {
				_ = os.Chmod(path, 0o644)
			}
			return nil
		})
	})
	return dir
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, rel, content string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func TestModeToggle(t *testing.T) {
	dir := initTempRepo(t)
	eng := NewEngine(dir)

	res, err := eng.Mode("status")
	if err != nil {
		t.Fatalf("Mode(status) failed: %v", err)
	}
	if !res.RDDEnabled {
		t.Fatalf("expected default RDDEnabled=true, got false")
	}

	res, err = eng.Mode("disable")
	if err != nil || res.RDDEnabled {
		t.Fatalf("Mode(disable) = (%+v, %v), want RDDEnabled=false", res, err)
	}

	cfg, err := config.LoadConfig(dir)
	if err != nil || cfg.RDDEnabled {
		t.Fatalf("LoadConfig after disable = (%+v, %v), want RDDEnabled=false", cfg, err)
	}

	res, err = eng.Mode("enable")
	if err != nil || !res.RDDEnabled {
		t.Fatalf("Mode(enable) = (%+v, %v), want RDDEnabled=true", res, err)
	}

	if _, err := eng.Mode("bogus"); err == nil {
		t.Fatal("expected error for invalid mode action")
	}
}

func TestAssessLiveCommittedAndEscalate(t *testing.T) {
	dir := initTempRepo(t)
	eng := NewEngine(dir)

	// Unborn repo with untracked Go file.
	writeFile(t, dir, "main.go", "package main\nfunc main() {\n\tprintln(\"hi\")\n}\n", 0o644)

	res, err := eng.Assess(AssessRequest{})
	if err != nil {
		t.Fatalf("Assess on unborn repo failed: %v", err)
	}
	if !res.RDDEnabled || !res.ReviewDue {
		t.Fatalf("expected RDDEnabled=true and ReviewDue=true, got %+v", res)
	}
	if res.NativeRisk == "passive" || res.Risk == "passive" {
		t.Fatalf("expected medium or high risk for Go file, got %+v", res)
	}
	if len(res.ChangedPaths) != 1 || res.ChangedPaths[0] != "main.go" {
		t.Fatalf("ChangedPaths = %v, want [main.go]", res.ChangedPaths)
	}
	if res.CandidateDigest == "" {
		t.Fatal("expected non-empty CandidateDigest")
	}

	// Ensure Assess is read-only (creates no rdd/candidates dirs).
	if _, err := os.Stat(filepath.Join(dir, ".git", "odd-agy", "candidates")); !os.IsNotExist(err) {
		t.Fatalf("Assess must be read-only, candidates dir exists or stat err: %v", err)
	}

	// Commit base and create docs-only commit (passive).
	runGit(t, dir, "add", "main.go")
	runGit(t, dir, "commit", "-m", "initial")
	baseSHA := runGit(t, dir, "rev-parse", "HEAD")

	writeFile(t, dir, "README.md", "# Docs\nHello\n", 0o644)
	runGit(t, dir, "add", "README.md")
	runGit(t, dir, "commit", "-m", "docs")

	// Reject BaseRef without CommittedOnly.
	if _, err := eng.Assess(AssessRequest{BaseRef: baseSHA, CommittedOnly: false}); err == nil {
		t.Fatal("expected error when BaseRef is set without CommittedOnly=true")
	}

	// Docs-only committed range should be passive.
	passiveRes, err := eng.Assess(AssessRequest{BaseRef: baseSHA, CommittedOnly: true})
	if err != nil {
		t.Fatalf("Assess committed docs failed: %v", err)
	}
	if passiveRes.NativeRisk != "passive" || passiveRes.Risk != "passive" || passiveRes.ReviewDue {
		t.Fatalf("expected passive without reviewDue, got %+v", passiveRes)
	}
	if passiveRes.IndependentVerifier {
		t.Fatal("expected IndependentVerifier=false without Escalate")
	}

	// Escalate validation (1..6, non-empty reason <= 500 UTF-8 bytes).
	for _, bad := range []Escalation{
		{Item: 0, Reason: "valid"},
		{Item: 7, Reason: "valid"},
		{Item: 2, Reason: "   "},
		{Item: 3, Reason: strings.Repeat("a", 501)},
	} {
		b := bad
		if _, err := eng.Assess(AssessRequest{BaseRef: baseSHA, CommittedOnly: true, Escalate: &b}); err == nil {
			t.Fatalf("expected validation error for escalate %+v", bad)
		}
	}

	// Valid Escalate raises Risk to high, sets ReviewDue=true, IndependentVerifier=true.
	escRes, err := eng.Assess(AssessRequest{
		BaseRef:       baseSHA,
		CommittedOnly: true,
		Escalate:      &Escalation{Item: 4, Reason: "Concurrency boundary check"},
	})
	if err != nil {
		t.Fatalf("Assess with valid Escalate failed: %v", err)
	}
	if escRes.NativeRisk != "passive" || escRes.Risk != "high" || !escRes.ReviewDue || !escRes.IndependentVerifier {
		t.Fatalf("unexpected escalate result: %+v", escRes)
	}
}

func TestFullLifecycleConsentDeclineAndTamperAndCorrectionAndAck(t *testing.T) {
	dir := initTempRepo(t)
	eng := NewEngine(dir)

	writeFile(t, dir, "app.go", "package app\n\nfunc Run() int {\n\treturn 1\n}\n", 0o644)
	runGit(t, dir, "add", "app.go")
	runGit(t, dir, "commit", "-m", "base")

	writeFile(t, dir, "app.go", "package app\n\nfunc Run() int {\n\treturn 2\n}\n", 0o644)
	writeFile(t, dir, "helper.go", "package app\n\nfunc Help() int {\n\treturn 3\n}\n", 0o644)
	writeFile(t, dir, "odd/tasks/spec.md", "# Spec\n", 0o644)

	// Start requires RDD enabled.
	if _, err := eng.Mode("disable"); err != nil {
		t.Fatalf("Mode(disable): %v", err)
	}
	if _, err := eng.Start(StartRequest{Lenses: []string{"risk"}, LensesReason: "check"}); err == nil {
		t.Fatal("expected Start to fail when RDD is disabled")
	}
	if _, err := eng.Mode("enable"); err != nil {
		t.Fatalf("Mode(enable): %v", err)
	}

	// Invalid lenses/reason rejected.
	if _, err := eng.Start(StartRequest{Lenses: []string{}, LensesReason: "check"}); err == nil {
		t.Fatal("expected error for empty lenses")
	}
	if _, err := eng.Start(StartRequest{Lenses: []string{"risk", "risk"}, LensesReason: "check"}); err == nil {
		t.Fatal("expected error for duplicate lenses")
	}
	if _, err := eng.Start(StartRequest{Lenses: []string{"unknown"}, LensesReason: "check"}); err == nil {
		t.Fatal("expected error for unknown lens")
	}
	if _, err := eng.Start(StartRequest{Lenses: []string{"risk"}, LensesReason: "   "}); err == nil {
		t.Fatal("expected error for empty lensesReason")
	}

	// 1) Decline consent path: candidate-scoped, does not disable project RDD.
	declinedStart, err := eng.Start(StartRequest{
		Lenses:       []string{"risk"},
		LensesReason: "decline test",
	})
	if err != nil {
		t.Fatalf("Start (decline path) failed: %v", err)
	}
	if !declinedStart.ConsentRequired || declinedStart.NextAction != "answer_consent" {
		t.Fatalf("unexpected StartResult: %+v", declinedStart)
	}
	declinedStatus, err := eng.AnswerConsent(declinedStart.LineageID, "declined")
	if err != nil {
		t.Fatalf("AnswerConsent(declined) failed: %v", err)
	}
	if declinedStatus.Status != "declined" {
		t.Fatalf("status = %q, want declined", declinedStatus.Status)
	}
	modeAfterDecline, err := eng.Mode("status")
	if err != nil || !modeAfterDecline.RDDEnabled {
		t.Fatalf("declining candidate must not disable project RDD: %+v, err=%v", modeAfterDecline, err)
	}

	// 2) Full review lifecycle with granted consent, scope pagination, tamper detection, refuter, 1-correction budget, and ack.
	startRes, err := eng.Start(StartRequest{
		Lenses:             []string{"risk", "reliability"},
		LensesReason:       "Core logic changed",
		RequestContextPath: "odd/tasks/spec.md",
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Cannot submit before consent granted.
	if _, err := eng.Submit(SubmitRequest{LineageID: startRes.LineageID, Lens: "risk"}); err == nil {
		t.Fatal("expected Submit to fail before consent granted")
	}

	status, err := eng.AnswerConsent(startRes.LineageID, "granted")
	if err != nil {
		t.Fatalf("AnswerConsent(granted) failed: %v", err)
	}
	if status.Status != "collecting_lenses" || len(status.PendingLenses) != 2 {
		t.Fatalf("unexpected status after consent granted: %+v", status)
	}

	// Scope pagination & worktree isolation: edit live worktree AFTER Start.
	writeFile(t, dir, "app.go", "package app\n\nfunc Run() int {\n\treturn 999\n}\n", 0o644)
	page1, err := eng.Scope(startRes.LineageID, 0, 2)
	if err != nil {
		t.Fatalf("Scope page 1 failed: %v", err)
	}
	// 4 changed files: .agents/odd/config.json (from Mode toggle), app.go, helper.go, odd/tasks/spec.md
	if page1.TotalFiles != 4 || len(page1.Files) != 2 || !page1.HasMore {
		t.Fatalf("unexpected page 1: total=%d len=%d hasMore=%v", page1.TotalFiles, len(page1.Files), page1.HasMore)
	}
	page2, err := eng.Scope(startRes.LineageID, page1.NextCursor, 2)
	if err != nil {
		t.Fatalf("Scope page 2 failed: %v", err)
	}
	if len(page2.Files) != 2 || page2.HasMore {
		t.Fatalf("unexpected page 2: len=%d hasMore=%v", len(page2.Files), page2.HasMore)
	}
	// Verify frozen content does NOT reflect live edit "999".
	for _, f := range append(page1.Files, page2.Files...) {
		if f.Path == "app.go" && strings.Contains(f.Content, "999") {
			t.Fatal("Scope leaked live worktree modification instead of frozen snapshot")
		}
	}

	// Submit risk lens with pre_existing severe + warning (non-blocking) and 1 candidate severe blocker.
	status, err = eng.Submit(SubmitRequest{
		LineageID: startRes.LineageID,
		Lens:      "risk",
		Findings: []Finding{
			{ID: "F-PRE", Severity: SeveritySEVERE, Causality: CausalityPreExisting, Location: "app.go:1", Claim: "pre-existing issue"},
			{ID: "F-WARN", Severity: SeverityWARNING, Causality: CausalityCandidate, Location: "helper.go:1", Claim: "minor style"},
			{ID: "F-BLK1", Severity: SeveritySEVERE, Causality: CausalityCandidate, Location: "app.go:3", Claim: "returns wrong constant"},
		},
	})
	if err != nil {
		t.Fatalf("Submit risk failed: %v", err)
	}
	if status.Status != "collecting_lenses" || len(status.Advisories) != 2 || len(status.Blockers) != 1 {
		t.Fatalf("unexpected status after risk lens: %+v", status)
	}

	// Submit reliability lens cleanly -> enters collecting_refuter because F-BLK1 exists.
	status, err = eng.Submit(SubmitRequest{
		LineageID: startRes.LineageID,
		Lens:      "reliability",
	})
	if err != nil {
		t.Fatalf("Submit reliability failed: %v", err)
	}
	if status.Status != "collecting_refuter" || status.NextAction != "submit_refuter" {
		t.Fatalf("expected collecting_refuter, got %+v", status)
	}

	// Refuter confirms F-BLK1 -> opens 1 bounded correction budget.
	status, err = eng.Submit(SubmitRequest{
		LineageID: startRes.LineageID,
		Lens:      "refuter",
		Verdicts: []RefuterVerdict{
			{FindingID: "F-BLK1", Verdict: VerdictConfirmed, Rationale: "confirmed bug"},
		},
	})
	if err != nil {
		t.Fatalf("Submit refuter failed: %v", err)
	}
	if status.Status != "correction_required" || status.CorrectionsUsed != 0 || status.CorrectionBudget != 1 {
		t.Fatalf("expected correction_required, got %+v", status)
	}

	// Ack must fail while correction_required.
	if _, err := eng.Ack(startRes.LineageID); err == nil {
		t.Fatal("expected Ack to fail before approved_pending_ack")
	}

	// Apply correction -> re-freezes rev 2 (captures the live 999 change), increments CorrectionsUsed to 1.
	status, err = eng.ApplyCorrection(startRes.LineageID)
	if err != nil {
		t.Fatalf("ApplyCorrection failed: %v", err)
	}
	if status.Status != "collecting_lenses" || status.Revision != 2 || status.CorrectionsUsed != 1 || len(status.PendingLenses) != 2 {
		t.Fatalf("unexpected status after ApplyCorrection: %+v", status)
	}

	// Verify Scope now shows rev 2 content ("999").
	rev2Page, err := eng.Scope(startRes.LineageID, 0, 10)
	if err != nil {
		t.Fatalf("Scope rev 2 failed: %v", err)
	}
	found999 := false
	for _, f := range rev2Page.Files {
		if f.Path == "app.go" && strings.Contains(f.Content, "999") {
			found999 = true
		}
	}
	if !found999 {
		t.Fatal("expected rev 2 frozen snapshot to contain corrected content 999")
	}

	// Pass rev 2 lenses cleanly -> approved_pending_ack.
	if _, err := eng.Submit(SubmitRequest{LineageID: startRes.LineageID, Lens: "risk"}); err != nil {
		t.Fatalf("Submit rev 2 risk failed: %v", err)
	}
	status, err = eng.Submit(SubmitRequest{LineageID: startRes.LineageID, Lens: "reliability"})
	if err != nil {
		t.Fatalf("Submit rev 2 reliability failed: %v", err)
	}
	if status.Status != "approved_pending_ack" || status.NextAction != "ack" {
		t.Fatalf("expected approved_pending_ack, got %+v", status)
	}

	// Ack burns authority and records CandidateDigest.
	ackRes, err := eng.Ack(startRes.LineageID)
	if err != nil {
		t.Fatalf("Ack failed: %v", err)
	}
	if ackRes.Status != "closed" || ackRes.Authority != "burned" || ackRes.DeliveryPolicy != "ordinary-repository-policy" {
		t.Fatalf("unexpected AckResult: %+v", ackRes)
	}

	// Assess on unchanged candidate reports AlreadyApproved=true.
	assessAfterAck, err := eng.Assess(AssessRequest{})
	if err != nil {
		t.Fatalf("Assess after Ack failed: %v", err)
	}
	if !assessAfterAck.AlreadyApproved || assessAfterAck.ReviewDue {
		t.Fatalf("expected AlreadyApproved=true and ReviewDue=false after Ack, got %+v", assessAfterAck)
	}

	// Modifying worktree invalidates AlreadyApproved.
	writeFile(t, dir, "app.go", "package app\n\nfunc Run() int {\n\treturn 1000\n}\n", 0o644)
	assessModified, err := eng.Assess(AssessRequest{})
	if err != nil {
		t.Fatalf("Assess after modification failed: %v", err)
	}
	if assessModified.AlreadyApproved || !assessModified.ReviewDue {
		t.Fatalf("expected AlreadyApproved=false and ReviewDue=true after edit, got %+v", assessModified)
	}

	// Tamper detection: alter frozen manifest.json and assert Scope fails closed.
	manifestPath := filepath.Join(dir, ".git", "odd-agy", "candidates", startRes.LineageID, "manifest.json")
	candDir := filepath.Dir(manifestPath)
	if err := os.Chmod(candDir, 0o755); err != nil {
		t.Fatalf("chmod candDir: %v", err)
	}
	if err := os.Chmod(manifestPath, 0o644); err != nil {
		t.Fatalf("chmod manifestPath: %v", err)
	}
	rawManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	tampered := strings.Replace(string(rawManifest), "999", "888", 1)
	if err := os.WriteFile(manifestPath, []byte(tampered), 0o444); err != nil {
		t.Fatalf("write tampered manifest: %v", err)
	}
	if _, err := eng.Scope(startRes.LineageID, 0, 10); err == nil {
		t.Fatal("expected Scope to fail closed when frozen manifest was tampered with")
	}

	// Double Ack must fail.
	if _, err := eng.Ack(startRes.LineageID); err == nil {
		t.Fatal("expected second Ack on closed lineage to fail")
	}
}

func TestHighRiskModeChangeAndDeletedFileAndFrozenFileTamper(t *testing.T) {
	dir := initTempRepo(t)
	eng := NewEngine(dir)

	writeFile(t, dir, "run.sh", "#!/bin/sh\necho hi\n", 0o644)
	writeFile(t, dir, "old.txt", "obsolete\n", 0o644)
	runGit(t, dir, "add", "run.sh", "old.txt")
	runGit(t, dir, "commit", "-m", "base")

	if err := os.Chmod(filepath.Join(dir, "run.sh"), 0o755); err != nil {
		t.Fatalf("chmod run.sh: %v", err)
	}
	runGit(t, dir, "rm", "old.txt")

	assessRes, err := eng.Assess(AssessRequest{})
	if err != nil {
		t.Fatalf("Assess failed: %v", err)
	}
	if assessRes.NativeRisk != RiskHigh || assessRes.Risk != RiskHigh {
		t.Fatalf("expected high risk for executable mode change, got %+v", assessRes)
	}

	st, err := eng.Start(StartRequest{Lenses: []string{"risk"}, LensesReason: "mode and deletion"})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if _, err := eng.AnswerConsent(st.LineageID, "granted"); err != nil {
		t.Fatalf("AnswerConsent failed: %v", err)
	}
	page, err := eng.Scope(st.LineageID, 0, 10)
	if err != nil {
		t.Fatalf("Scope failed: %v", err)
	}
	if page.TotalFiles != 2 {
		t.Fatalf("expected 2 files in scope, got %d", page.TotalFiles)
	}

	// Tamper with frozen file copy on disk (leaving manifest.json untouched).
	frozenFile := filepath.Join(dir, ".git", "odd-agy", "candidates", st.LineageID, "rev-1", "files", "run.sh")
	frozenDir := filepath.Dir(frozenFile)
	if err := os.Chmod(frozenDir, 0o755); err != nil {
		t.Fatalf("chmod frozenDir: %v", err)
	}
	if err := os.Chmod(frozenFile, 0o644); err != nil {
		t.Fatalf("chmod frozenFile: %v", err)
	}
	if err := os.WriteFile(frozenFile, []byte("#!/bin/sh\necho pwned\n"), 0o444); err != nil {
		t.Fatalf("overwrite frozenFile: %v", err)
	}
	if _, err := eng.Scope(st.LineageID, 0, 10); err == nil {
		t.Fatal("expected Scope to fail closed when frozen file on disk was tampered with")
	}
}

func TestSecondBlockerEscalatesAndUnknownEscalatesAndRefutedApproves(t *testing.T) {
	dir := initTempRepo(t)
	eng := NewEngine(dir)
	writeFile(t, dir, "svc.go", "package svc\nfunc Do() {}\n", 0o644)

	// Case A: 2nd confirmed blocker after 1 correction -> escalated.
	s1, err := eng.Start(StartRequest{Lenses: []string{"risk"}, LensesReason: "check"})
	if err != nil {
		t.Fatalf("Start A: %v", err)
	}
	if _, err := eng.AnswerConsent(s1.LineageID, "granted"); err != nil {
		t.Fatalf("consent A: %v", err)
	}
	if _, err := eng.Submit(SubmitRequest{
		LineageID: s1.LineageID,
		Lens:      "risk",
		Findings:  []Finding{{ID: "B1", Severity: SeveritySEVERE, Causality: CausalityCandidate, Location: "svc.go:1", Claim: "bug 1"}},
	}); err != nil {
		t.Fatalf("submit 1 A: %v", err)
	}
	if _, err := eng.Submit(SubmitRequest{
		LineageID: s1.LineageID,
		Lens:      "refuter",
		Verdicts:  []RefuterVerdict{{FindingID: "B1", Verdict: VerdictConfirmed}},
	}); err != nil {
		t.Fatalf("refuter 1 A: %v", err)
	}
	if _, err := eng.ApplyCorrection(s1.LineageID); err != nil {
		t.Fatalf("ApplyCorrection A: %v", err)
	}
	if _, err := eng.Submit(SubmitRequest{
		LineageID: s1.LineageID,
		Lens:      "risk",
		Findings:  []Finding{{ID: "B2", Severity: SeveritySEVERE, Causality: CausalityCandidate, Location: "svc.go:1", Claim: "bug 2"}},
	}); err != nil {
		t.Fatalf("submit 2 A: %v", err)
	}
	stA, err := eng.Submit(SubmitRequest{
		LineageID: s1.LineageID,
		Lens:      "refuter",
		Verdicts:  []RefuterVerdict{{FindingID: "B2", Verdict: VerdictConfirmed}},
	})
	if err != nil {
		t.Fatalf("refuter 2 A: %v", err)
	}
	if stA.Status != "escalated" {
		t.Fatalf("expected escalated on 2nd blocker, got %+v", stA)
	}
	if _, err := eng.ApplyCorrection(s1.LineageID); err == nil {
		t.Fatal("expected ApplyCorrection to fail when escalated / budget exhausted")
	}

	// Case B: refuter unknown -> escalated.
	s2, err := eng.Start(StartRequest{Lenses: []string{"risk"}, LensesReason: "check unknown"})
	if err != nil {
		t.Fatalf("Start B: %v", err)
	}
	if _, err := eng.AnswerConsent(s2.LineageID, "granted"); err != nil {
		t.Fatalf("consent B: %v", err)
	}
	if _, err := eng.Submit(SubmitRequest{
		LineageID: s2.LineageID,
		Lens:      "risk",
		Findings:  []Finding{{ID: "U1", Severity: SeveritySEVERE, Causality: CausalityUnknown, Location: "svc.go:1", Claim: "unclear"}},
	}); err != nil {
		t.Fatalf("submit B: %v", err)
	}
	stB, err := eng.Submit(SubmitRequest{
		LineageID: s2.LineageID,
		Lens:      "refuter",
		Verdicts:  []RefuterVerdict{{FindingID: "U1", Verdict: VerdictUnknown}},
	})
	if err != nil {
		t.Fatalf("refuter B: %v", err)
	}
	if stB.Status != "escalated" {
		t.Fatalf("expected escalated on unknown causality, got %+v", stB)
	}

	// Case C: all blockers refuted -> approved_pending_ack.
	s3, err := eng.Start(StartRequest{Lenses: []string{"risk"}, LensesReason: "check refuted"})
	if err != nil {
		t.Fatalf("Start C: %v", err)
	}
	if _, err := eng.AnswerConsent(s3.LineageID, "granted"); err != nil {
		t.Fatalf("consent C: %v", err)
	}
	if _, err := eng.Submit(SubmitRequest{
		LineageID: s3.LineageID,
		Lens:      "risk",
		Findings:  []Finding{{ID: "R1", Severity: SeveritySEVERE, Causality: CausalityCandidate, Location: "svc.go:1", Claim: "false alarm"}},
	}); err != nil {
		t.Fatalf("submit C: %v", err)
	}
	stC, err := eng.Submit(SubmitRequest{
		LineageID: s3.LineageID,
		Lens:      "refuter",
		Verdicts:  []RefuterVerdict{{FindingID: "R1", Verdict: VerdictRefuted}},
	})
	if err != nil {
		t.Fatalf("refuter C: %v", err)
	}
	if stC.Status != "approved_pending_ack" {
		t.Fatalf("expected approved_pending_ack when all refuted, got %+v", stC)
	}
}
