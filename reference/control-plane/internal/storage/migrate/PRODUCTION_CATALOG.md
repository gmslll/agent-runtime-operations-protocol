# Production migration catalog closure

`CurrentProductionCatalog` is the only migration declaration used by the real
Reference Control Plane composition. It is a fail-closed static-input contract,
not a directory-discovery convenience.

For P09, the declaration contains only the two P09 base migrations and points
to the P09 report. The P09 composition acceptance copies those two digest-bound
files into an isolated catalog root before invoking the real composition. Files
added to the repository by a later phase therefore cannot retroactively enter
the P09 runtime or its report.

Every later Control Plane persistence phase must update the declaration in the
same change that adds its paired SQLite/PostgreSQL migration:

1. include the complete production history through the current phase;
2. set `ReportPhase` and `ReportPath` to that phase and list the exact
   persistence phases admitted by `OwnerPhases`;
3. list the owner phase and SHA-256 for every migration in both dialects;
4. include this package and every listed SQL file in that phase report's static
   input closure; and
5. run the real composition against an isolated copy of exactly that closure.

The loader rejects missing files, undeclared files, digest drift, duplicate or
non-canonical paths, dialect asymmetry, invalid report provenance, and any
migration whose owner phase is later than the declared report phase. Production
composition never falls back to unrestricted directory loading.
