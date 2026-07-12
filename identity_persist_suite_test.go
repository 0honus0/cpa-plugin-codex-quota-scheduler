package main

import "testing"

func TestSuiteIdentityPersist(t *testing.T) {
	t.Run("identity", TestCredentialClassification)
	t.Run("wal", TestCredentialRecoveryReconcilesUnknownOutcome)
	t.Run("state", TestStateStoreBackupAndDualCorruptionRecovery)
	t.Run("fence", TestFenceCrashAfterCeilingPersistenceCreatesSafeGap)
}
func TestMockGroupAIdentityPersist(t *testing.T) { TestSuiteIdentityPersist(t) }
func TestMockGroupDIdentitySecurity(t *testing.T) {
	t.Run("classification", TestCredentialClassification)
	t.Run("sensitive", TestStateStoreContainsNoSensitiveTerms)
	t.Run("stale-instance", TestCredentialSaveRejectsOldInstance)
}
