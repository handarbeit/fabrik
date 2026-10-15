# ADR 2305: Skill catalogue, resolver and load-failure detector live in `internal/skillref`

## Status

Accepted

## Context

Fabrik resolves `plugin:skill` references against the plugins it loads (ADR 2137, 2140) and detects a skill that fails to load in a worker (ADR 2138). Pruefer needs the same capabilities for its own review tooling, but must not import `engine` (ADR 1113). The code sat in `engine/skill_catalogue.go`, `engine/skill_resolve.go` and `engine/skill_load_detect.go`.

## Decision

1. **A leaf package.** `internal/skillref` holds plugin discovery (`Discover`, `Build`, `Catalogue`), resolution (`Catalogue.Resolve`, `Resolver` with its rebuild on a miss, `UnresolvedError`) and the stream-json detector (`DetectLoadFailure`, `SkillNamesMatch`, `SanitizeDetail`). It imports only the standard library, `plugin` and `internal/claudeerr`. R1 names "plugin and the standard library"; `claudeerr` is a stdlib-only leaf and is read as included, because the detector returns `*claudeerr.SkillLoadError`, the type the engine already aliases. `deps_test.go` fails on any other module import.
2. **Consumer differences are `Options`, not constants.** `PreferredBases` is an ordered list (not a single name), because Fabrik prefers `fabrik-stages` and then a customised pre-#2140 `fabrik`; a single name would silently lose that. `LegacyRef` is a function with the signature of `plugin.LegacyRef`; nil means no legacy mapping. The order of lookup is unchanged: as-is first, legacy only on a miss, a legacy hit flagged `Deprecated`. The zero `Options` is valid (load order, no legacy), which is what Pruefer will pass apart from its own base.
3. **The engine keeps its behaviour through thin wiring.** `fabrikSkillOptions` and the one-argument `newSkillResolver(dirs)` in `engine/skill_resolve.go` supply Fabrik's values; type aliases (`skillResolver`, `skillCatalogue`, `skillUnresolvedError`, `resolvedSkill`) keep every `errors.As`, composite literal and field use compiling. `resolveStageSkills`, `stageSkillRefs`, the handlers, warnings and labels stay in `engine/`. `pluginManifestRel` aliases `skillref.ManifestRel`; the copy in `cmd/upgrade_migrate.go` is left alone.
4. **The sanitiser is duplicated, with a parity test.** `SanitizeDetail` repeats the 200/140/40 head/tail truncation of the engine's `sanitizeToolsDeniedCommand`/`truncateMiddle`, which other engine code shares and which therefore cannot move. `TestSkillrefSanitizeDetailMatchesToolsDeniedSanitiser` pins the two to identical output so they cannot drift.
5. **Tests move with the code.** The catalogue, resolver and detector tests, and the real-capture fixture (now `internal/skillref/testdata/failed_skill_call_real.ndjson`), live in the new package and run with Fabrik-equivalent options so the legacy cases are not vacuous; zero-`Options` and nil-safety cases were added. The engine keeps the tests of its wrappers, and a wiring test that fails if `newSkillResolver` drops a base name or the legacy table.

## Consequences

- ADR 2138 cites `engine/testdata/skill_load/…` and `engine/skill_load_detect.go`, and ADR 2140 cites `skillCatalogue.resolve`; they are historical records and are not rewritten. This ADR is the pointer to the new locations.
- Pruefer can adopt the package in the next issue of this chain without touching `engine`.
- Precedents: `internal/claudeerr` (ADR 1449), `internal/sessionreap` (ADR 1989).
