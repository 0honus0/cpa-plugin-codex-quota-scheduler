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
