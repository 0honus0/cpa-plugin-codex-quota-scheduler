# Refactor deviations

| ID | Spec clause | Conflict | Conservative decision | Tests | Status |
| --- | --- | --- | --- | --- | --- |
| DEV-001 | Plan Task 1 RED | The Resource/Management boundary already satisfies INV-01, so forcing a production-code failure would weaken known-good behavior. | Characterization test + mutation validation replaces forced RED. S1 onward remains strict RED -> GREEN. | `TestSuiteBoundary/mutation_scanner_detects_a_test-owned_leak` | Accepted |
