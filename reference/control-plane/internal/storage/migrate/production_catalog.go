package migrate

// CurrentProductionCatalog is deliberately explicit. A later persistence
// phase must add both dialect files, advance ReportPhase/ReportPath, and bind
// this source plus all listed SQL files in that phase's static report inputs in
// the same change.
func CurrentProductionCatalog() CatalogClosure {
	return CatalogClosure{
		ID: "control-plane-production-migrations", ReportPhase: "P09", ReportPath: "build/reports/P09/report.json",
		OwnerPhases: []string{"P09"},
		Migrations: []DeclaredMigration{
			{Dialect: DialectPostgres, Path: "postgres/0001_base.sql", OwnerPhase: "P09", SHA256: "c9f04000d5ce26ee7d86d3b89131ac05861945f5e95537c52ffa6cf359388707"},
			{Dialect: DialectSQLite, Path: "sqlite/0001_base.sql", OwnerPhase: "P09", SHA256: "e7873b3f504595272913eb66b4656cbcb157ce901b1fd823bf9967ea64c5b348"},
		},
	}
}
