# Doctor checks

`internal/doctor` implements the non-mutating checks and opt-in repairs used by `term-llm doctor`. User-facing command documentation lives at [Check the install with `term-llm doctor`](https://term-llm.com/guides/debugging/#check-the-install-with-term-llm-doctor).

## Adding a check

Implement `doctor.Check` in `internal/doctor`:

```go
type Check interface {
    ID() string
    Title() string
    Run(ctx context.Context) []Finding
}
```

Rules:

- `Run` must not mutate user state. Attach repairs to the finding's `Fix` callback instead; the runner decides whether to call it.
- A repair must be safe to skip and must back up anything it rewrites.
- Give every non-`ok` finding a concrete `Remedy`. A report full of vague warnings gets ignored.
- Register the check in `doctorChecks` in `cmd/doctor.go`, which owns path resolution so checks stay testable with injected paths, and extend `TestDoctorChecksCoverEveryDocumentedArea`.

Progress reporting is driven by `doctor.Options.Observer`, which the runner calls once before each check and once after its confirmed repairs have been applied. A check does not print progress itself.
