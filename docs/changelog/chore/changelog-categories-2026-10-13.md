# Changelog categories

## Summary

Sam wants to read past changes in messaging by type. Before, all 37 entries sat loose at the top of `docs/changelog/`, with the date first in their names. Now every entry sits in a folder named for the type of change, such as `docs/changelog/feat/`.

## Design

37 entries moved into `feat`, `fix`, `perf`, `refactor`, `chore`, `docs`, `test`, `ci`, `security` or `revert` by what the change does. Every name now ends in its date: `{short-name}-YYYY-MM-DD.md`. The rule and the category table live in a new `CLAUDE.md`, the same rule as the astro repo.

## Migration

None. New changelogs go to `docs/changelog/{category}/{short-name}-YYYY-MM-DD.md`.
