package migrate

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	catalogIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{2,63}$`)
	phasePattern     = regexp.MustCompile(`^P([0-9]{2})$`)
	digestPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// CatalogClosure is the production migration input contract for one phase.
// Every migration that a real composition may read must be listed with the
// digest that the producing phase report records as a static input.
type CatalogClosure struct {
	ID          string
	ReportPhase string
	ReportPath  string
	OwnerPhases []string
	Migrations  []DeclaredMigration
}

type DeclaredMigration struct {
	Dialect    Dialect
	Path       string
	OwnerPhase string
	SHA256     string
}

// LoadCatalogClosure validates the complete dual-database declaration before
// reading the selected dialect. It fails closed when the on-disk directory has
// missing or undeclared files, so a binary/report cannot silently consume a
// migration owned by a later phase.
func LoadCatalogClosure(filesystem fs.FS, closure CatalogClosure, dialect Dialect) (*Catalog, error) {
	if filesystem == nil {
		return nil, errors.New("migration catalog filesystem is required")
	}
	if dialect != DialectSQLite && dialect != DialectPostgres {
		return nil, errors.New("unsupported migration dialect")
	}
	byDialect, err := validateCatalogClosure(closure)
	if err != nil {
		return nil, err
	}
	loaded := map[Dialect]*Catalog{}
	for _, declaredDialect := range []Dialect{DialectSQLite, DialectPostgres} {
		catalog, loadErr := loadDeclaredDialect(filesystem, declaredDialect, byDialect[declaredDialect])
		if loadErr != nil {
			return nil, loadErr
		}
		loaded[declaredDialect] = catalog
	}
	return loaded[dialect], nil
}

func loadDeclaredDialect(filesystem fs.FS, dialect Dialect, declared []DeclaredMigration) (*Catalog, error) {
	directory := string(dialect)
	if err := validateCatalogDirectory(filesystem, directory); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(filesystem, directory)
	if err != nil {
		return nil, fmt.Errorf("read declared migration catalog: %w", err)
	}
	actual := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return nil, fmt.Errorf("migration catalog contains non-regular entry %q", entry.Name())
		}
		if migrationFilePattern.FindStringSubmatch(entry.Name()) == nil {
			return nil, fmt.Errorf("migration catalog contains unexpected file %q", entry.Name())
		}
		actual = append(actual, path.Join(directory, entry.Name()))
	}
	sort.Strings(actual)
	want := make([]string, len(declared))
	for index, item := range declared {
		want[index] = item.Path
	}
	if !equalStrings(actual, want) {
		return nil, fmt.Errorf("migration catalog inventory is outside declared closure: actual=%v declared=%v", actual, want)
	}
	migrations := make([]Migration, 0, len(declared))
	for _, item := range declared {
		contents, readErr := fs.ReadFile(filesystem, item.Path)
		if readErr != nil {
			return nil, fmt.Errorf("read declared migration %q: %w", item.Path, readErr)
		}
		match := migrationFilePattern.FindStringSubmatch(path.Base(item.Path))
		version, _ := strconv.ParseInt(match[1], 10, 64)
		migration, migrationErr := NewMigration(version, match[2], contents)
		if migrationErr != nil {
			return nil, fmt.Errorf("load declared migration %q: %w", item.Path, migrationErr)
		}
		if migration.Checksum != item.SHA256 {
			return nil, fmt.Errorf("declared migration %q does not match its report input digest", item.Path)
		}
		migrations = append(migrations, migration)
	}
	return NewCatalog(dialect, migrations)
}

func validateCatalogClosure(closure CatalogClosure) (map[Dialect][]DeclaredMigration, error) {
	if !catalogIDPattern.MatchString(closure.ID) {
		return nil, errors.New("migration catalog closure ID is invalid")
	}
	reportNumber, err := phaseNumber(closure.ReportPhase)
	if err != nil {
		return nil, fmt.Errorf("migration catalog report phase: %w", err)
	}
	wantReportPath := "build/reports/" + closure.ReportPhase + "/report.json"
	if closure.ReportPath != wantReportPath {
		return nil, fmt.Errorf("migration catalog report path must equal %q", wantReportPath)
	}
	allowedOwners := map[string]bool{}
	priorOwner := 0
	for _, owner := range closure.OwnerPhases {
		ownerNumber, ownerErr := phaseNumber(owner)
		if ownerErr != nil || ownerNumber > reportNumber || ownerNumber <= priorOwner {
			return nil, errors.New("migration catalog owner phases must be unique, increasing, and not later than the report phase")
		}
		allowedOwners[owner] = true
		priorOwner = ownerNumber
	}
	if len(allowedOwners) == 0 || !allowedOwners[closure.ReportPhase] {
		return nil, errors.New("migration catalog owner phases must include the report phase")
	}
	byDialect := map[Dialect][]DeclaredMigration{DialectSQLite: {}, DialectPostgres: {}}
	seenPaths := map[string]bool{}
	for _, item := range closure.Migrations {
		if item.Dialect != DialectSQLite && item.Dialect != DialectPostgres {
			return nil, errors.New("declared migration has unsupported dialect")
		}
		ownerNumber, ownerErr := phaseNumber(item.OwnerPhase)
		if ownerErr != nil {
			return nil, fmt.Errorf("declared migration %q owner phase: %w", item.Path, ownerErr)
		}
		if ownerNumber > reportNumber {
			return nil, fmt.Errorf("declared migration %q belongs to future phase %s after report phase %s", item.Path, item.OwnerPhase, closure.ReportPhase)
		}
		if !allowedOwners[item.OwnerPhase] {
			return nil, fmt.Errorf("declared migration %q owner phase %s is outside the catalog scope", item.Path, item.OwnerPhase)
		}
		if item.Path == "" || path.IsAbs(item.Path) || path.Clean(item.Path) != item.Path || strings.HasPrefix(item.Path, "../") {
			return nil, fmt.Errorf("declared migration path %q is invalid", item.Path)
		}
		prefix := string(item.Dialect) + "/"
		if !strings.HasPrefix(item.Path, prefix) || strings.Contains(strings.TrimPrefix(item.Path, prefix), "/") {
			return nil, fmt.Errorf("declared migration path %q is outside its dialect directory", item.Path)
		}
		if migrationFilePattern.FindStringSubmatch(path.Base(item.Path)) == nil {
			return nil, fmt.Errorf("declared migration path %q is not canonical", item.Path)
		}
		if !digestPattern.MatchString(item.SHA256) {
			return nil, fmt.Errorf("declared migration %q digest is invalid", item.Path)
		}
		if seenPaths[item.Path] {
			return nil, fmt.Errorf("declared migration path %q is duplicated", item.Path)
		}
		seenPaths[item.Path] = true
		byDialect[item.Dialect] = append(byDialect[item.Dialect], item)
	}
	for dialect, items := range byDialect {
		if len(items) == 0 {
			return nil, fmt.Errorf("migration catalog closure has no %s migrations", dialect)
		}
		sort.Slice(items, func(left, right int) bool { return items[left].Path < items[right].Path })
		byDialect[dialect] = items
	}
	if err := validatePairedCatalogs(byDialect); err != nil {
		return nil, err
	}
	return byDialect, nil
}

func validatePairedCatalogs(byDialect map[Dialect][]DeclaredMigration) error {
	sqliteItems, postgresItems := byDialect[DialectSQLite], byDialect[DialectPostgres]
	if len(sqliteItems) != len(postgresItems) {
		return errors.New("SQLite and PostgreSQL migration closures have different lengths")
	}
	for index := range sqliteItems {
		sqliteName := strings.TrimPrefix(sqliteItems[index].Path, "sqlite/")
		postgresName := strings.TrimPrefix(postgresItems[index].Path, "postgres/")
		if sqliteName != postgresName || sqliteItems[index].OwnerPhase != postgresItems[index].OwnerPhase {
			return fmt.Errorf("SQLite and PostgreSQL migration closures diverge at index %d", index)
		}
	}
	return nil
}

func phaseNumber(phase string) (int, error) {
	match := phasePattern.FindStringSubmatch(phase)
	if match == nil {
		return 0, errors.New("phase must use PNN form")
	}
	number, err := strconv.Atoi(match[1])
	if err != nil || number < 1 {
		return 0, errors.New("phase number must be positive")
	}
	return number, nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
