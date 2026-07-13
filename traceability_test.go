package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var invariantTagPattern = regexp.MustCompile(`^//inv:(INV-[0-9]{2}(?:,INV-[0-9]{2})*) (positive|negative)$`)

type testFunction struct {
	Name string
	File string
	Decl *ast.FuncDecl
}

type mockCoverageMatrix struct {
	Schema string            `json:"schema"`
	Rows   []mockCoverageRow `json:"rows"`
}

type mockCoverageRow struct {
	ID       string   `json:"id"`
	Group    string   `json:"group"`
	Scenario string   `json:"scenario"`
	Owners   []string `json:"owners"`
}

func isExecutableTestFunc(fn *ast.FuncDecl) bool {
	if fn == nil || fn.Recv != nil || fn.Body == nil || fn.Type.Results != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
		return false
	}
	params := fn.Type.Params
	if params == nil || len(params.List) != 1 || len(params.List[0].Names) != 1 {
		return false
	}
	star, ok := params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	selector, ok := star.X.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "T"
}

var frozenSection12RowIDs = []string{
	"A01", "A02", "A03", "A04", "A05", "A06", "A07", "A08", "A09", "A10",
	"B01", "B02", "B03", "B04",
	"C01", "C02", "C03", "C04", "C05", "C06",
	"D01", "D02",
	"E01", "E02", "E03", "E04", "E05", "E06",
}

// invariantExplicitOwners is reserved for evidence whose behavioral test is
// clearer to name directly than to annotate in place. Every entry is checked
// against the AST-discovered executable Test functions before it is accepted.
var invariantExplicitOwners = map[string][]string{
	"INV-02 positive": {"TestProductionProvisionalRequestMarkerEndToEnd"},
	"INV-02 negative": {"TestProductionProvisionalVerificationRejectsMismatchWithoutOpenAI"},
	"INV-03 positive": {"TestAuthoritativeDurableGenerationSurvivesFailureObservation"},
	"INV-03 negative": {"TestRosterRemovalCancelsInFlightAndFencesWriteback"},
	"INV-06 positive": {"TestTypedProbeSendNeverJoinsSameAttempt"},
	"INV-06 negative": {"TestTypedProbeSendNeverJoinsSameAttempt"},
	"INV-07 positive": {"TestProbeSequenceHoldsInstanceLeaseThroughVerify"},
	"INV-07 negative": {"TestTypedPropagationLeaseReclaimEntersSentUnknownWithoutResign"},
	"INV-08 positive": {"TestProbeDueFailureContinuesOtherConfirmedInstance"},
	"INV-08 negative": {"TestProbeRecoveryFailureContinuesOtherConfirmedInstance"},
	"INV-10 positive": {"TestNormalRefreshDeadlineHasSingleControllerOwner"},
	"INV-10 negative": {"TestSuiteRefresh"},
	"INV-11 positive": {"TestSuiteRefresh"},
	"INV-11 negative": {"TestSuiteRefresh"},
	"INV-12 positive": {"TestMockGroupB"},
	"INV-12 negative": {"TestProductionEvidenceQueueAndDynamicTrial"},
	"INV-13 positive": {"TestPickAllowsWeeklyWhenExhaustedFiveHourResetPassed"},
	"INV-13 negative": {"TestPickSkipsWeeklyWhenFiveHourExhausted"},
	"INV-14 positive": {"TestProductionProbeRunsWhileNormalRefreshDormant"},
	"INV-14 negative": {"TestProductionProbeRunsWhileNormalRefreshDormant"},
	"INV-15 positive": {"TestPluginStateHalfOpenSuccessClosesCircuit"},
	"INV-15 negative": {"TestProductionProbeRunsWhileNormalRefreshDormant"},
	"INV-16 positive": {"TestUsageQuotaFailureMarksTemporaryExhaustedWithoutOpeningCircuit"},
	"INV-16 negative": {"TestUsageQuotaFailureMarksTemporaryExhaustedWithoutOpeningCircuit"},
	"INV-17 positive": {"TestProbeControllerDualWindowIndependent"},
	"INV-17 negative": {"TestProbeAllStateEventsAndDualWindowProduct"},
	"INV-18 positive": {"TestProbeTimeJumpsProduceAtMostOneSequence"},
	"INV-18 negative": {"TestProbeDeadlineConsumedOnceWithoutSpin"},
	"INV-19 positive": {"TestProbeControllerPersistentStateSetAndIllegalNoop"},
	"INV-19 negative": {"TestStateStoreBackupAndDualCorruptionRecovery"},
	"INV-20 negative": {"TestRosterRemovalCancelsInFlightAndFencesWriteback"},
	"INV-21 positive": {"TestAuthoritativeDurableGenerationSurvivesFailureObservation"},
	"INV-21 negative": {"TestFailClosedHoldsAndRecoveryRecomputesProbe"},
	"INV-22 positive": {"TestSuiteRefresh"},
	"INV-22 negative": {"TestSuiteRefresh"},
	"INV-27 positive": {"TestProductionProbeKPointCrashRestartVerifyFirst"},
	"INV-27 negative": {"TestProbeRecoveryWaitsForGraceAndNeverResendsDuringSuppression"},
	"INV-28 positive": {"TestBindingAdmissionEpochMonotonicAcrossDeleteReaddAndRestart"},
	"INV-28 negative": {"TestCredentialSaveRejectsOldInstance"},
	"INV-29 positive": {"TestTrialRegistryCASAndEvidence"},
	"INV-29 negative": {"TestProductionEvidenceQueueAndDynamicTrial"},
	"INV-30 positive": {"TestStateStoreBackupAndDualCorruptionRecovery"},
	"INV-30 negative": {"TestRuntimeAndUserArtifactsNeverPersistSensitiveValues"},
	"INV-32 positive": {"TestProductionProbeRunsWhileNormalRefreshDormant"},
	"INV-32 negative": {"TestSuiteRefresh"},
	"INV-33 positive": {"TestProbeAuthBlockedResumesOnlyAfterExternalLoginEpoch"},
	"INV-36 positive": {"TestProductionProbeKPointCrashRestartVerifyFirst"},
	"INV-36 negative": {"TestProbeRecoveryWaitsForGraceAndNeverResendsDuringSuppression"},
	"INV-37 positive": {"TestAnnotationsEndpointsNormalizePatchAndPersist"},
	"INV-37 negative": {"TestAnnotationsPersistenceFailureDoesNotMutateMemory"},
	"INV-38 positive": {"TestFenceBlockExhaustionAndRestartMonotonicity"},
	"INV-38 negative": {"TestFenceDoesNotIssueWhenCeilingPersistenceFails"},
	"INV-39 positive": {"TestFencePersistsCeilingBeforeIssuing"},
	"INV-39 negative": {"TestFenceDoesNotIssueWhenCeilingPersistenceFails"},
	"INV-41 positive": {"TestTrialPendingAtSixtySecondsAndBudget"},
	"INV-41 negative": {"TestTrialThreeRetriesForceUnknown"},
	"INV-43 positive": {"TestSchedulerPickABIPathSnapshotOnly"},
	"INV-43 negative": {"TestSchedulerPickObservesOneImmutablePublication"},
	"INV-44 positive": {"TestSchedulerPickRefreshesOnlyActivePriorityCandidates"},
	"INV-44 negative": {"TestRosterCandidatesHaveNoRosterSideEffects"},
	"INV-45 positive": {"TestTrialPendingAtSixtySecondsAndBudget"},
	"INV-45 negative": {"TestTrialThreeRetriesForceUnknown"},
}

func discoverTestFunctions(root string) (map[string]testFunction, error) {
	files, err := filepath.Glob(filepath.Join(root, "*_test.go"))
	if err != nil {
		return nil, err
	}
	functions := make(map[string]testFunction)
	for _, name := range files {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !isExecutableTestFunc(fn) {
				continue
			}
			functions[fn.Name.Name] = testFunction{Name: fn.Name.Name, File: name, Decl: fn}
		}
	}
	return functions, nil
}

func scanInvariantEvidence(root string, explicit map[string][]string) (map[string][]string, error) {
	functions, err := discoverTestFunctions(root)
	if err != nil {
		return nil, err
	}
	evidence := make(map[string][]string)
	files, err := filepath.Glob(filepath.Join(root, "*_test.go"))
	if err != nil {
		return nil, err
	}
	for _, name := range files {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !isExecutableTestFunc(fn) {
				continue
			}
			for _, group := range file.Comments {
				if group.Pos() < fn.Body.Lbrace || group.End() > fn.Body.Rbrace {
					continue
				}
				for _, comment := range group.List {
					match := invariantTagPattern.FindStringSubmatch(strings.TrimSpace(comment.Text))
					if match == nil {
						continue
					}
					for _, id := range strings.Split(match[1], ",") {
						key := id + " " + match[2]
						evidence[key] = append(evidence[key], fn.Name.Name)
					}
				}
			}
		}
	}
	for key, owners := range explicit {
		if !regexp.MustCompile(`^INV-[0-9]{2} (positive|negative)$`).MatchString(key) {
			return nil, fmt.Errorf("invalid invariant mapping key %q", key)
		}
		for _, owner := range owners {
			if !strings.HasPrefix(owner, "Test") {
				return nil, fmt.Errorf("invariant %s owner must name Test function: %q", key, owner)
			}
			if _, ok := functions[owner]; !ok {
				return nil, fmt.Errorf("invariant %s unknown test owner %q", key, owner)
			}
			evidence[key] = append(evidence[key], owner)
		}
	}
	for key := range evidence {
		sort.Strings(evidence[key])
		evidence[key] = slicesCompact(evidence[key])
	}
	return evidence, nil
}

func slicesCompact(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func loadMockCoverage(path string) (mockCoverageMatrix, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return mockCoverageMatrix{}, err
	}
	var matrix mockCoverageMatrix
	if err := json.Unmarshal(raw, &matrix); err != nil {
		return mockCoverageMatrix{}, err
	}
	return matrix, nil
}

func validateMockCoverage(root string, matrix mockCoverageMatrix) error {
	if matrix.Schema != "section-12-v2" {
		return fmt.Errorf("mock coverage schema = %q, want section-12-v2", matrix.Schema)
	}
	functions, err := discoverTestFunctions(root)
	if err != nil {
		return err
	}
	expected := make(map[string]struct{}, len(frozenSection12RowIDs))
	for _, id := range frozenSection12RowIDs {
		expected[id] = struct{}{}
	}
	seen := make(map[string]struct{}, len(matrix.Rows))
	var problems []string
	for _, row := range matrix.Rows {
		if _, ok := expected[row.ID]; !ok {
			problems = append(problems, fmt.Sprintf("unknown section-12 row %q", row.ID))
			continue
		}
		if _, duplicate := seen[row.ID]; duplicate {
			problems = append(problems, fmt.Sprintf("duplicate section-12 row %s", row.ID))
		}
		seen[row.ID] = struct{}{}
		if len(row.Owners) == 0 {
			problems = append(problems, fmt.Sprintf("section-12 row %s missing owner", row.ID))
		}
		for _, owner := range row.Owners {
			if !strings.HasPrefix(owner, "Test") {
				problems = append(problems, fmt.Sprintf("section-12 row %s owner must name Test function: %q", row.ID, owner))
			} else if _, ok := functions[owner]; !ok {
				problems = append(problems, fmt.Sprintf("section-12 row %s unknown test owner %q", row.ID, owner))
			}
		}
		if row.Group == "" || row.Scenario == "" {
			problems = append(problems, fmt.Sprintf("section-12 row %s missing group/scenario description", row.ID))
		}
	}
	for _, id := range frozenSection12RowIDs {
		if _, ok := seen[id]; !ok {
			problems = append(problems, fmt.Sprintf("missing section-12 row %s", id))
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		return fmt.Errorf("invalid mock coverage:\n%s", strings.Join(problems, "\n"))
	}
	return nil
}

func TestInvariantTraceability(t *testing.T) {
	evidence, err := scanInvariantEvidence(".", invariantExplicitOwners)
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	for i := 1; i <= 46; i++ {
		id := fmt.Sprintf("INV-%02d", i)
		for _, direction := range []string{"positive", "negative"} {
			key := id + " " + direction
			if len(evidence[key]) == 0 {
				missing = append(missing, key)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("missing invariant traceability:\n%s", strings.Join(missing, "\n"))
	}
}

func TestMockCoverage(t *testing.T) {
	matrix, err := loadMockCoverage(filepath.Join("testdata", "mock_group_coverage.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateMockCoverage(".", matrix); err != nil {
		t.Fatal(err)
	}
}

func TestInvariantTraceabilityRejectsAggregationOnlyTags(t *testing.T) {
	root := t.TempDir()
	source := `package fixture

//inv:INV-01 positive
//inv:INV-01 negative
func TestBehavior(t *testing.T) {}
`
	if err := os.WriteFile(filepath.Join(root, "aggregation_test.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	evidence, err := scanInvariantEvidence(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, direction := range []string{"positive", "negative"} {
		key := "INV-01 " + direction
		if owners := evidence[key]; len(owners) != 0 {
			t.Fatalf("aggregation-only tag %q was accepted by owners %v", key, owners)
		}
	}
}

func TestMockCoverageRejectsMissingOwner(t *testing.T) {
	root := t.TempDir()
	source := `package fixture

import "testing"

func TestExistingBehavior(t *testing.T) {}
func TestWrongSignature() {}
func helperBehavior(t *testing.T) {}
`
	if err := os.WriteFile(filepath.Join(root, "behavior_test.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		owner string
		want  string
	}{
		{name: "missing", owner: "", want: "missing owner"},
		{name: "unknown", owner: "TestDoesNotExist", want: "unknown test owner"},
		{name: "non-executable", owner: "TestWrongSignature", want: "unknown test owner"},
		{name: "non-Test", owner: "helperBehavior", want: "owner must name Test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := make([]mockCoverageRow, 0, len(frozenSection12RowIDs))
			for _, id := range frozenSection12RowIDs {
				rows = append(rows, mockCoverageRow{ID: id, Group: id[:1], Scenario: "fixture", Owners: []string{"TestExistingBehavior"}})
			}
			if tc.owner == "" {
				rows[0].Owners = nil
			} else {
				rows[0].Owners = []string{tc.owner}
			}
			matrix := mockCoverageMatrix{Schema: "section-12-v2", Rows: rows}
			err := validateMockCoverage(root, matrix)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateMockCoverage() error = %v, want substring %q", err, tc.want)
			}
		})
	}
}
