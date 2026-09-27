package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"testing"
	"testing/fstest"
)

func TestCatalogClosure(t *testing.T) {
	sqliteSQL := []byte("CREATE TABLE closure_probe(id INTEGER PRIMARY KEY);\n")
	postgresSQL := []byte("CREATE TABLE closure_probe(id BIGINT PRIMARY KEY);\n")
	validFS := fstest.MapFS{
		"sqlite/0001_base.sql":   &fstest.MapFile{Data: sqliteSQL, Mode: 0o644},
		"postgres/0001_base.sql": &fstest.MapFile{Data: postgresSQL, Mode: 0o644},
	}
	valid := CatalogClosure{
		ID: "production-migrations", ReportPhase: "P09", ReportPath: "build/reports/P09/report.json", OwnerPhases: []string{"P09"},
		Migrations: []DeclaredMigration{
			{Dialect: DialectSQLite, Path: "sqlite/0001_base.sql", OwnerPhase: "P09", SHA256: testDigest(sqliteSQL)},
			{Dialect: DialectPostgres, Path: "postgres/0001_base.sql", OwnerPhase: "P09", SHA256: testDigest(postgresSQL)},
		},
	}

	t.Run("declared-complete-static-input-closure", func(t *testing.T) {
		for _, dialect := range []Dialect{DialectSQLite, DialectPostgres} {
			catalog, err := LoadCatalogClosure(validFS, valid, dialect)
			if err != nil {
				t.Fatalf("%s closure rejected: %v", dialect, err)
			}
			if catalog.TargetVersion() != 1 {
				t.Fatalf("%s target=%d want=1", dialect, catalog.TargetVersion())
			}
		}
	})

	t.Run("missing-declared-migration", func(t *testing.T) {
		filesystem := cloneMapFS(validFS)
		delete(filesystem, "postgres/0001_base.sql")
		if _, err := LoadCatalogClosure(filesystem, valid, DialectSQLite); err == nil {
			t.Fatal("missing paired-dialect migration was accepted")
		}
	})

	t.Run("undeclared-migration", func(t *testing.T) {
		filesystem := cloneMapFS(validFS)
		filesystem["sqlite/0002_future.sql"] = &fstest.MapFile{Data: []byte("SELECT 2;\n"), Mode: 0o644}
		if _, err := LoadCatalogClosure(filesystem, valid, DialectSQLite); err == nil {
			t.Fatal("undeclared migration was accepted")
		}
	})

	t.Run("report-input-digest-drift", func(t *testing.T) {
		filesystem := cloneMapFS(validFS)
		filesystem["sqlite/0001_base.sql"] = &fstest.MapFile{Data: []byte("SELECT 9;\n"), Mode: 0o644}
		if _, err := LoadCatalogClosure(filesystem, valid, DialectSQLite); err == nil {
			t.Fatal("migration outside the reported digest was accepted")
		}
	})

	t.Run("future-owner-phase", func(t *testing.T) {
		closure := valid
		closure.Migrations = append([]DeclaredMigration(nil), valid.Migrations...)
		closure.Migrations[0].OwnerPhase = "P10"
		if _, err := LoadCatalogClosure(validFS, closure, DialectSQLite); err == nil {
			t.Fatal("future-phase migration was accepted")
		}
	})

	t.Run("owner-phase-outside-scope", func(t *testing.T) {
		closure := valid
		closure.Migrations = append([]DeclaredMigration(nil), valid.Migrations...)
		closure.Migrations[0].OwnerPhase = "P08"
		if _, err := LoadCatalogClosure(validFS, closure, DialectSQLite); err == nil {
			t.Fatal("migration from an undeclared owner phase was accepted")
		}
	})

	t.Run("report-provenance-mismatch", func(t *testing.T) {
		closure := valid
		closure.ReportPath = "build/reports/P10/report.json"
		if _, err := LoadCatalogClosure(validFS, closure, DialectSQLite); err == nil {
			t.Fatal("mismatched report provenance was accepted")
		}
	})

	t.Run("cross-dialect-version-closure", func(t *testing.T) {
		closure := valid
		closure.Migrations = append([]DeclaredMigration(nil), valid.Migrations...)
		closure.Migrations[1].Path = "postgres/0002_other.sql"
		if _, err := LoadCatalogClosure(validFS, closure, DialectPostgres); err == nil {
			t.Fatal("asymmetric dual-database closure was accepted")
		}
	})
}

func TestProductionCatalogSnapshots(t *testing.T) {
	p09 := P09ProductionCatalog()
	if p09.ReportPhase != "P09" || p09.ReportPath != "build/reports/P09/report.json" || len(p09.Migrations) != 2 {
		t.Fatalf("P09 snapshot drifted: %+v", p09)
	}
	p10 := P10ProductionCatalog()
	if p10.ReportPhase != "P10" || p10.ReportPath != "build/reports/P10/report.json" || len(p10.Migrations) != 4 {
		t.Fatalf("P10 snapshot drifted: %+v", p10)
	}
	p12 := P12ProductionCatalog()
	if p12.ReportPhase != "P12" || p12.ReportPath != "build/reports/P12/report.json" || len(p12.Migrations) != 6 {
		t.Fatalf("P12 snapshot drifted: %+v", p12)
	}
	p13 := P13ProductionCatalog()
	if p13.ReportPhase != "P13" || p13.ReportPath != "build/reports/P13/report.json" || len(p13.Migrations) != 8 {
		t.Fatalf("P13 snapshot drifted: %+v", p13)
	}
	p14 := P14ProductionCatalog()
	if p14.ReportPhase != "P14" || p14.ReportPath != "build/reports/P14/report.json" || len(p14.Migrations) != 10 {
		t.Fatalf("P14 snapshot drifted: %+v", p14)
	}
	p18 := P18ProductionCatalog()
	if p18.ReportPhase != "P18" || p18.ReportPath != "build/reports/P18/report.json" || len(p18.Migrations) != 12 {
		t.Fatalf("P18 snapshot drifted: %+v", p18)
	}
	p19 := P19ProductionCatalog()
	if p19.ReportPhase != "P19" || p19.ReportPath != "build/reports/P19/report.json" || len(p19.Migrations) != 14 {
		t.Fatalf("P19 snapshot drifted: %+v", p19)
	}
	p20 := P20ProductionCatalog()
	if p20.ReportPhase != "P20" || len(p20.Migrations) != 16 || !slices.Contains(p20.OwnerPhases, "P20") {
		t.Fatalf("unexpected P20 closure: %#v", p20)
	}
	p21 := P21ProductionCatalog()
	if p21.ReportPhase != "P21" || len(p21.Migrations) != 16 || !slices.Contains(p21.OwnerPhases, "P21") {
		t.Fatalf("unexpected P21 closure: %#v", p21)
	}
	p22 := P22ProductionCatalog()
	if p22.ReportPhase != "P22" || len(p22.Migrations) != 16 || !slices.Contains(p22.OwnerPhases, "P22") {
		t.Fatalf("unexpected P22 closure: %#v", p22)
	}
	current := CurrentProductionCatalog()
	if current.ReportPhase != "P24" || current.ReportPath != "build/reports/P24/report.json" || len(current.Migrations) != 18 || !slices.Contains(current.OwnerPhases, "P24") {
		t.Fatalf("current catalog is not P24 complete: %+v", current)
	}
	for _, item := range current.Migrations {
		if item.Path == "" || item.OwnerPhase == "" || item.SHA256 == "" {
			t.Fatalf("incomplete current declaration: %+v", item)
		}
	}
}

func testDigest(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

func cloneMapFS(source fstest.MapFS) fstest.MapFS {
	clone := fstest.MapFS{}
	for name, file := range source {
		copy := *file
		copy.Data = append([]byte(nil), file.Data...)
		clone[name] = &copy
	}
	return clone
}
