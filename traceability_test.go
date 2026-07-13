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
	"unicode"
	"unicode/utf8"
)

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

func isExecutableTestFunc(fn *ast.FuncDecl, testingAliases map[string]struct{}) bool {
	if fn == nil || fn.Recv != nil || fn.Body == nil || fn.Type.Results != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
		return false
	}
	if suffix := strings.TrimPrefix(fn.Name.Name, "Test"); suffix != "" {
		first, _ := utf8.DecodeRuneInString(suffix)
		if unicode.IsLower(first) {
			return false
		}
	}
	params := fn.Type.Params
	if params == nil || len(params.List) != 1 {
		return false
	}
	if len(params.List[0].Names) > 1 {
		return false
	}
	star, ok := params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	selector, ok := star.X.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "T" {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok {
		return false
	}
	_, ok = testingAliases[pkg.Name]
	return ok
}

var frozenSection12RowIDs = []string{
	"A01", "A02", "A03", "A04", "A05", "A06", "A07", "A08", "A09", "A10",
	"B01", "B02", "B03", "B04",
	"C01", "C02", "C03", "C04", "C05", "C06",
	"D01", "D02",
	"E01", "E02", "E03", "E04", "E05", "E06",
}

var frozenSection12Rows = map[string]struct {
	Group    string
	Scenario string
}{
	"A01": {"A", "Probe sent then crash before sent WAL recovers verify-first"},
	"A02": {"A", "Crash after sending WAL and before HTTP has SentUnknown semantics"},
	"A03": {"A", "Fence reservation crash points preserve durable ceiling safety"},
	"A04": {"A", "Credential F0 to F1 to F2 skip and out-of-order observations reconcile"},
	"A05": {"A", "Delete and re-add identity combinations advance admission fencing safely"},
	"A06": {"A", "Thirty-minute degraded boundary fails background closed while real pick remains independent"},
	"A07": {"A", "Legacy envelope interleaves safely with manual and quota intents"},
	"A08": {"A", "All five Probe baselines plus UsageOnly table rows"},
	"A09": {"A", "Window lengths cross reset rollback jump plausibility and missing values"},
	"A10": {"A", "Concurrent picks retain one trial and force TrialUnknown at evidence budget"},
	"B01": {"B", "All 1440 single-instance scheduling vectors match independent oracle"},
	"B02": {"B", "All representative multi-instance vectors and candidate relations match oracle"},
	"B03": {"B", "Two-to-four concurrent picks preserve single trial and hot-path safety"},
	"B04": {"B", "Deterministic property-style scheduling vectors compare with independent oracle"},
	"C01": {"C", "All legal and illegal Probe state-event transitions"},
	"C02": {"C", "Full Probe classifier Cartesian golden grid"},
	"C03": {"C", "Ten-by-ten dual-window isolation product"},
	"C04": {"C", "Probe crash points restart to convergent persisted state"},
	"C05": {"C", "Time jumps across zero one and multiple deadlines emit at most one sequence"},
	"C06": {"C", "Normal recovery and SentUnknown suppression entrances cross verify outcomes"},
	"D01": {"D", "Credential transition order SaveAuth outcomes and identity ambiguity boundaries"},
	"D02": {"D", "Resource endpoints contain no runtime-sensitive account data"},
	"E01": {"E", "Two-to-three event coordinator interleavings preserve dedupe barriers fencing and lock order"},
	"E02": {"E", "Refresh timeline boundaries and source-priority truth table"},
	"E03": {"E", "Business circuit transitions remain isolated from Probe and usage-limit feedback"},
	"E04": {"E", "Degraded failure boundary and authoritative recovery"},
	"E05": {"E", "Lease returns before and after reclaim across operation classes"},
	"E06": {"E", "S3 drain completion and timeout paths fence old work and inherit uncertain Probe"},
}

// invariantExplicitOwners is reserved for evidence whose behavioral test is
// clearer to name directly than to annotate in place. Every entry is checked
// against the AST-discovered executable Test functions before it is accepted.
var invariantExplicitOwners = map[string][]string{
	"INV-01 positive": {"TestSuiteBoundary"},
	"INV-01 negative": {"TestResourceBoundaryRejectsSensitiveBusinessDataLeak"},
	"INV-02 positive": {"TestProductionProvisionalRequestMarkerEndToEnd"},
	"INV-02 negative": {"TestProductionProvisionalVerificationRejectsMismatchWithoutOpenAI"},
	"INV-03 positive": {"TestAuthoritativeDurableGenerationSurvivesFailureObservation"},
	"INV-03 negative": {"TestRosterRemovalCancelsInFlightAndFencesWriteback"},
	"INV-04 positive": {"TestCredentialSaveWALOrdering"},
	"INV-04 negative": {"TestBindingValidatorRejectsEveryStaleComponent"},
	"INV-05 positive": {"TestTypedQuotaReadAllocatesAtActualStartAndBarrierJoin"},
	"INV-05 negative": {"TestSection12A07LegacyManualQuotaConcurrent"},
	"INV-06 positive": {"TestProductionProbeRunsWhileNormalRefreshDormant"},
	"INV-06 negative": {"TestTypedProbeSendNeverJoinsSameAttempt"},
	"INV-07 positive": {"TestProbeSequenceHoldsInstanceLeaseThroughVerify"},
	"INV-07 negative": {"TestTypedPropagationLeaseReclaimEntersSentUnknownWithoutResign"},
	"INV-08 positive": {"TestProbeDueFailureContinuesOtherConfirmedInstance"},
	"INV-08 negative": {"TestProbeRecoveryFailureContinuesOtherConfirmedInstance"},
	"INV-09 positive": {"TestTypedPropagationWaitRetainsLeaseWithoutSlot"},
	"INV-09 negative": {"TestLegacyProbeMarksSendOnlyAfterHTTPSlotAcquired"},
	"INV-10 positive": {"TestNormalRefreshDeadlineHasSingleControllerOwner"},
	"INV-10 negative": {"TestStaleAfterAloneNeverTriggersRequest"},
	"INV-11 positive": {"TestAgingCacheClassificationEmitsNoRequest"},
	"INV-11 negative": {"TestAgingCannotOwnRefreshDeadline"},
	"INV-12 positive": {"TestInvariant12FirstRealRequestSingleRefresh"},
	"INV-12 negative": {"TestInvariant12ConcurrentRequestsRejectDuplicateRefresh"},
	"INV-13 positive": {"TestPickAllowsWeeklyWhenExhaustedFiveHourResetPassed"},
	"INV-13 negative": {"TestPickSkipsWeeklyWhenFiveHourExhausted"},
	"INV-14 positive": {"TestProbeControllerDormantDeadlineStillEmitsProbe"},
	"INV-14 negative": {"TestProductionProbeRunsWhileNormalRefreshDormant"},
	"INV-15 positive": {"TestPluginStateHalfOpenSuccessClosesCircuit"},
	"INV-15 negative": {"TestProductionProbeRunsWhileNormalRefreshDormant"},
	"INV-16 positive": {"TestUsageQuotaFailureMarksTemporaryExhaustedWithoutOpeningCircuit"},
	"INV-16 negative": {"TestUsageHandleMarksTemporaryExhaustedByAuthIndex"},
	"INV-17 positive": {"TestProbeControllerDualWindowIndependent"},
	"INV-17 negative": {"TestProbeAllStateEventsAndDualWindowProduct"},
	"INV-18 positive": {"TestProbeTimeJumpsProduceAtMostOneSequence"},
	"INV-18 negative": {"TestProbeDeadlineConsumedOnceWithoutSpin"},
	"INV-19 positive": {"TestProbeControllerPersistentStateSetAndIllegalNoop"},
	"INV-19 negative": {"TestStateStoreBackupAndDualCorruptionRecovery"},
	"INV-20 positive": {"TestManagementUsesActiveRosterOnly"},
	"INV-20 negative": {"TestRosterRemovalCancelsInFlightAndFencesWriteback"},
	"INV-21 positive": {"TestAuthoritativeDurableGenerationSurvivesFailureObservation"},
	"INV-21 negative": {"TestFailClosedHoldsAndRecoveryRecomputesProbe"},
	"INV-22 positive": {"TestStatusPayloadNotesStaleAccountWhileSleeping"},
	"INV-22 negative": {"TestRefreshDueOnceDoesNothingOutsideActiveWindow"},
	"INV-23 positive": {"TestProbeAuthBlockedResumesOnlyAfterExternalLoginEpoch"},
	"INV-23 negative": {"TestBindingGenesisFailsClosedForCapabilityBAndAuthBlocked"},
	"INV-24 positive": {"TestRefreshSourcePriorityTruthTable"},
	"INV-24 negative": {"TestLegacyRefreshCompleteOutboundEnvelope"},
	"INV-25 positive": {"TestRosterRemovalCancelsInFlightAndFencesWriteback"},
	"INV-25 negative": {"TestRefreshAuthDiscardsInFlightSuccessAfterPriorityGenerationChange"},
	"INV-26 positive": {"TestTypedQuotaReadAllocatesAtActualStartAndBarrierJoin"},
	"INV-26 negative": {"TestProbeVerifyRejectsReadAtOrBeforeSendFence"},
	"INV-27 positive": {"TestProductionProbeKPointCrashRestartVerifyFirst"},
	"INV-27 negative": {"TestProbeRecoveryWaitsForGraceAndNeverResendsDuringSuppression"},
	"INV-28 positive": {"TestBindingAdmissionEpochMonotonicAcrossDeleteReaddAndRestart"},
	"INV-28 negative": {"TestCredentialSaveRejectsOldInstance"},
	"INV-29 positive": {"TestTrialRegistryCASAndEvidence"},
	"INV-29 negative": {"TestSection12B03ConcurrentPicksOneTrial"},
	"INV-30 positive": {"TestStateStoreBackupAndDualCorruptionRecovery"},
	"INV-30 negative": {"TestRuntimeAndUserArtifactsNeverPersistSensitiveValues"},
	"INV-31 positive": {"TestSuiteRosterManagement"},
	"INV-31 negative": {"TestConcurrentSameMomentRosterWakesSingleflight"},
	"INV-32 positive": {"TestProductionProbeRunsWhileNormalRefreshDormant"},
	"INV-32 negative": {"TestRefreshDueOnceDoesNothingOutsideActiveWindow"},
	"INV-33 positive": {"TestProbeAuthBlockedResumesOnlyAfterExternalLoginEpoch"},
	"INV-33 negative": {"TestCredentialAmbiguityManagementExitsEpochSemantics"},
	"INV-34 positive": {"TestManagementUsesActiveRosterOnly"},
	"INV-34 negative": {"TestRosterCandidatesHaveNoRosterSideEffects"},
	"INV-35 positive": {"TestProductionRosterLifecycleGatesBackgroundRequests"},
	"INV-35 negative": {"TestFailClosedHoldsAndRecoveryRecomputesProbe"},
	"INV-36 positive": {"TestProductionProbeKPointCrashRestartVerifyFirst"},
	"INV-36 negative": {"TestProbeRecoveryWaitsForGraceAndNeverResendsDuringSuppression"},
	"INV-37 positive": {"TestAnnotationsEndpointsNormalizePatchAndPersist"},
	"INV-37 negative": {"TestAnnotationsPersistenceFailureDoesNotMutateMemory"},
	"INV-38 positive": {"TestFenceBlockExhaustionAndRestartMonotonicity"},
	"INV-38 negative": {"TestFenceDoesNotIssueWhenCeilingPersistenceFails"},
	"INV-39 positive": {"TestFencePersistsCeilingBeforeIssuing"},
	"INV-39 negative": {"TestFenceDoesNotIssueWhenCeilingPersistenceFails"},
	"INV-40 positive": {"TestSection12D01CredentialIdentityMatrix"},
	"INV-40 negative": {"TestCredentialSaveFailureAndUnknown"},
	"INV-41 positive": {"TestTrialPendingAtSixtySecondsAndBudget"},
	"INV-41 negative": {"TestTrialThreeRetriesForceUnknown"},
	"INV-42 positive": {"TestLegacyRefreshCompleteOutboundEnvelope"},
	"INV-42 negative": {"TestCoordinatorDrainCancelsQueuedWithoutStarting"},
	"INV-43 positive": {"TestSchedulerPickABIPathSnapshotOnly"},
	"INV-43 negative": {"TestSchedulerPickPublishesWhileOlderHostCallIsBlocked"},
	"INV-44 positive": {"TestSchedulerPickRefreshesOnlyActivePriorityCandidates"},
	"INV-44 negative": {"TestRosterCandidatesHaveNoRosterSideEffects"},
	"INV-45 positive": {"TestTrialPendingAtSixtySecondsAndBudget"},
	"INV-45 negative": {"TestTrialThreeRetriesForceUnknown"},
	"INV-46 positive": {"TestLegacyDrainSentUnknownPersistsAcrossRestart"},
	"INV-46 negative": {"TestCoordinatorSentUnknownPersistenceFailureBlocksDrainHandoff"},
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
		testingAliases := make(map[string]struct{})
		for _, spec := range file.Imports {
			if strings.Trim(spec.Path.Value, `"`) != "testing" {
				continue
			}
			alias := "testing"
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			if alias != "." && alias != "_" {
				testingAliases[alias] = struct{}{}
			}
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !isExecutableTestFunc(fn, testingAliases) {
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
	// Inline tags are deliberately not evidence: they are too easy to bulk-dump
	// into an aggregation test. The semantic manifest below must name executable
	// behavioral owners explicitly.
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
	for i := 1; i <= 46; i++ {
		id := fmt.Sprintf("INV-%02d", i)
		positive := make(map[string]struct{}, len(evidence[id+" positive"]))
		for _, owner := range evidence[id+" positive"] {
			positive[owner] = struct{}{}
		}
		for _, owner := range evidence[id+" negative"] {
			if _, duplicate := positive[owner]; duplicate {
				return nil, fmt.Errorf("invariant %s positive and negative owners must be distinct: %s", id, owner)
			}
		}
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
		frozen := frozenSection12Rows[row.ID]
		if row.Group != frozen.Group {
			problems = append(problems, fmt.Sprintf("section-12 row %s group = %q, want %q", row.ID, row.Group, frozen.Group))
		}
		if row.Scenario != frozen.Scenario {
			problems = append(problems, fmt.Sprintf("section-12 row %s scenario drift", row.ID))
		}
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

func TestInvariantTraceabilityRejectsCentralizedTestTagBlocks(t *testing.T) {
	root := t.TempDir()
	source := `package fixture

import "testing"

func TestBehavior(t *testing.T) {
	//inv:INV-01 positive
	//inv:INV-01 negative
	//inv:INV-02 positive
	//inv:INV-02 negative
	if 1 + 1 != 2 { t.Fatal("arithmetic") }
}
`
	if err := os.WriteFile(filepath.Join(root, "aggregation_test.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	evidence, err := scanInvariantEvidence(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 0 {
		t.Fatalf("centralized test-body tag block was accepted: %#v", evidence)
	}
}

func TestInvariantManifestRejectsSameDirectionalOwner(t *testing.T) {
	root := t.TempDir()
	source := `package fixture
import "testing"
func TestBehavior(t *testing.T) { if false { t.Fatal("guard") } }
`
	if err := os.WriteFile(filepath.Join(root, "behavior_test.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := scanInvariantEvidence(root, map[string][]string{
		"INV-01 positive": {"TestBehavior"},
		"INV-01 negative": {"TestBehavior"},
	})
	if err == nil || !strings.Contains(err.Error(), "distinct") {
		t.Fatalf("scanInvariantEvidence() error=%v, want distinct-owner rejection", err)
	}
}

func TestExecutableTestDiscoveryRequiresRealTestingImportAndExportedName(t *testing.T) {
	root := t.TempDir()
	fixtures := map[string]string{
		"missing_import_test.go": `package fixture
func TestMissingImport(t *testing.T) {}
`,
		"fake_testing_test.go": `package fixture
type fakeT struct{}
func TestFakeTesting(t *fakeT) {}
`,
		"lowercase_test.go": `package fixture
import "testing"
func Testlowercase(t *testing.T) {}
`,
		"grouped_test.go": `package fixture
import "testing"
func TestGrouped(a, b *testing.T) {}
`,
		"valid_test.go": `package fixture
import t "testing"
func TestValid(tb *t.T) {}
`,
	}
	for name, source := range fixtures {
		if err := os.WriteFile(filepath.Join(root, name), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	functions, err := discoverTestFunctions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(functions) != 1 {
		t.Fatalf("discovered tests = %v, want only TestValid", functions)
	}
	if _, ok := functions["TestValid"]; !ok {
		t.Fatalf("aliased real testing import was not resolved: %v", functions)
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

func TestMockCoverageRejectsGroupAndScenarioDrift(t *testing.T) {
	matrix, err := loadMockCoverage(filepath.Join("testdata", "mock_group_coverage.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*mockCoverageRow)
		want string
	}{
		{name: "group", edit: func(row *mockCoverageRow) { row.Group = "E" }, want: "group"},
		{name: "scenario", edit: func(row *mockCoverageRow) { row.Scenario = "hand-written drift" }, want: "scenario"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copyMatrix := matrix
			copyMatrix.Rows = append([]mockCoverageRow(nil), matrix.Rows...)
			tc.edit(&copyMatrix.Rows[0])
			err := validateMockCoverage(".", copyMatrix)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateMockCoverage() error = %v, want %q", err, tc.want)
			}
		})
	}
}
