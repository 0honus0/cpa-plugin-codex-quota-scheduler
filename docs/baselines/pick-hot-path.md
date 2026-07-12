# Scheduler Pick Hot-Path Baseline

- Date: 2026-07-12
- Source baseline commit: `db15d2d` with the S1 working-tree implementation
- Platform: Windows/amd64, 13th Gen Intel(R) Core(TM) i9-13900H
- Command: `go test -bench BenchmarkSchedulerPickSnapshot -benchmem -run '^$'`

```text
BenchmarkSchedulerPickSnapshot-20       896913       1244 ns/op       2312 B/op       6 allocs/op
PASS
ok      github.com/jeffery/codex-quota-scheduler    2.663s
```

The fixture is published before timing starts. The benchmark calls only the
snapshot selector and performs no host, network, disk, sleep, or background
wait operation.

S1 also records existing transitive legacy pick-path debt in
`scripts/refactor_gates/s1-pick-path-baseline.json`. A standard-library Go AST
analyzer starts at real `handleSchedulerPick`, follows the same-package call
closure across files, resolves import aliases, and emits stable
file/type/symbol/count entries. The ratchet permits no new entry or count
increase; S5 must reduce the baseline to an empty array and pass the real ABI
snapshot-only test.
