package migrate

// P09ProductionCatalog is the immutable historical catalog used when the P09
// composition acceptance is replayed under later phases. It must never include
// a migration owned after P09.
func P09ProductionCatalog() CatalogClosure {
	return CatalogClosure{
		ID: "control-plane-p09-production-migrations", ReportPhase: "P09", ReportPath: "build/reports/P09/report.json",
		OwnerPhases: []string{"P09"},
		Migrations: []DeclaredMigration{
			{Dialect: DialectPostgres, Path: "postgres/0001_base.sql", OwnerPhase: "P09", SHA256: "c9f04000d5ce26ee7d86d3b89131ac05861945f5e95537c52ffa6cf359388707"},
			{Dialect: DialectSQLite, Path: "sqlite/0001_base.sql", OwnerPhase: "P09", SHA256: "e7873b3f504595272913eb66b4656cbcb157ce901b1fd823bf9967ea64c5b348"},
		},
	}
}

// CurrentProductionCatalog is the only catalog used by production
// composition. P10 advances the complete paired history to 0005 and binds it
// to the P10 report without admitting any future production migration.
func CurrentProductionCatalog() CatalogClosure {
	return CatalogClosure{
		ID: "control-plane-production-migrations", ReportPhase: "P10", ReportPath: "build/reports/P10/report.json",
		OwnerPhases: []string{"P09", "P10"},
		Migrations: []DeclaredMigration{
			{Dialect: DialectPostgres, Path: "postgres/0001_base.sql", OwnerPhase: "P09", SHA256: "c9f04000d5ce26ee7d86d3b89131ac05861945f5e95537c52ffa6cf359388707"},
			{Dialect: DialectPostgres, Path: "postgres/0005_identity.sql", OwnerPhase: "P10", SHA256: "5d0211ac031d98dd3760701bc19dddd2d96d67cfc74dc5140db6b18ddda75dde"},
			{Dialect: DialectSQLite, Path: "sqlite/0001_base.sql", OwnerPhase: "P09", SHA256: "e7873b3f504595272913eb66b4656cbcb157ce901b1fd823bf9967ea64c5b348"},
			{Dialect: DialectSQLite, Path: "sqlite/0005_identity.sql", OwnerPhase: "P10", SHA256: "192ce590834794c68be040237a7611eed2631edf166d6c9fbd9930e63a1cf6ce"},
		},
	}
}
