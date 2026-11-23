# Development priorities

- Treat throughput and allocations as API design constraints. Compare
  representative benchmarks before and after changes to hot paths, and report
  material regressions.
- Keep test coverage high. Use `make test` to check coverage by package. Cover
  new behavior and meaningful edge cases, and remove tests that add neither
  coverage nor meaningful verification.
