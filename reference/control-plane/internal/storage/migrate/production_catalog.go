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

// P10ProductionCatalog is the immutable historical catalog used when P10 is
// replayed under later phases. It must never include a migration owned after
// P10.
func P10ProductionCatalog() CatalogClosure {
	return CatalogClosure{
		ID: "control-plane-p10-production-migrations", ReportPhase: "P10", ReportPath: "build/reports/P10/report.json",
		OwnerPhases: []string{"P09", "P10"},
		Migrations: []DeclaredMigration{
			{Dialect: DialectPostgres, Path: "postgres/0001_base.sql", OwnerPhase: "P09", SHA256: "c9f04000d5ce26ee7d86d3b89131ac05861945f5e95537c52ffa6cf359388707"},
			{Dialect: DialectPostgres, Path: "postgres/0005_identity.sql", OwnerPhase: "P10", SHA256: "5d0211ac031d98dd3760701bc19dddd2d96d67cfc74dc5140db6b18ddda75dde"},
			{Dialect: DialectSQLite, Path: "sqlite/0001_base.sql", OwnerPhase: "P09", SHA256: "e7873b3f504595272913eb66b4656cbcb157ce901b1fd823bf9967ea64c5b348"},
			{Dialect: DialectSQLite, Path: "sqlite/0005_identity.sql", OwnerPhase: "P10", SHA256: "192ce590834794c68be040237a7611eed2631edf166d6c9fbd9930e63a1cf6ce"},
		},
	}
}

// P12ProductionCatalog is the immutable historical catalog used when P12 is
// replayed under later phases. It must never include a migration owned after
// P12.
func P12ProductionCatalog() CatalogClosure {
	return CatalogClosure{
		ID: "control-plane-p12-production-migrations", ReportPhase: "P12", ReportPath: "build/reports/P12/report.json",
		OwnerPhases: []string{"P09", "P10", "P12"},
		Migrations: []DeclaredMigration{
			{Dialect: DialectPostgres, Path: "postgres/0001_base.sql", OwnerPhase: "P09", SHA256: "c9f04000d5ce26ee7d86d3b89131ac05861945f5e95537c52ffa6cf359388707"},
			{Dialect: DialectPostgres, Path: "postgres/0005_identity.sql", OwnerPhase: "P10", SHA256: "5d0211ac031d98dd3760701bc19dddd2d96d67cfc74dc5140db6b18ddda75dde"},
			{Dialect: DialectPostgres, Path: "postgres/0010_publication.sql", OwnerPhase: "P12", SHA256: "4d09c8ea3744a805c111d8c6942a6ee128aa374cc249cb818f0327ea13927a13"},
			{Dialect: DialectSQLite, Path: "sqlite/0001_base.sql", OwnerPhase: "P09", SHA256: "e7873b3f504595272913eb66b4656cbcb157ce901b1fd823bf9967ea64c5b348"},
			{Dialect: DialectSQLite, Path: "sqlite/0005_identity.sql", OwnerPhase: "P10", SHA256: "192ce590834794c68be040237a7611eed2631edf166d6c9fbd9930e63a1cf6ce"},
			{Dialect: DialectSQLite, Path: "sqlite/0010_publication.sql", OwnerPhase: "P12", SHA256: "524364c706ca6e336f5de0dd42058772699b7255f53cf099e96bef10e4973df8"},
		},
	}
}

// CurrentProductionCatalog is the only catalog used by production
// composition. P13 advances the paired history to 0020. Historical P09, P10,
// and P12 acceptance always inject their immutable snapshot instead.
func CurrentProductionCatalog() CatalogClosure {
	return CatalogClosure{
		ID: "control-plane-production-migrations", ReportPhase: "P13", ReportPath: "build/reports/P13/report.json",
		OwnerPhases: []string{"P09", "P10", "P12", "P13"},
		Migrations: []DeclaredMigration{
			{Dialect: DialectPostgres, Path: "postgres/0001_base.sql", OwnerPhase: "P09", SHA256: "c9f04000d5ce26ee7d86d3b89131ac05861945f5e95537c52ffa6cf359388707"},
			{Dialect: DialectPostgres, Path: "postgres/0005_identity.sql", OwnerPhase: "P10", SHA256: "5d0211ac031d98dd3760701bc19dddd2d96d67cfc74dc5140db6b18ddda75dde"},
			{Dialect: DialectPostgres, Path: "postgres/0010_publication.sql", OwnerPhase: "P12", SHA256: "4d09c8ea3744a805c111d8c6942a6ee128aa374cc249cb818f0327ea13927a13"},
			{Dialect: DialectPostgres, Path: "postgres/0020_asset.sql", OwnerPhase: "P13", SHA256: "e6e57df2db19bb1fc95885de9ac39abb5389a239fd7ad139394832491a0411ed"},
			{Dialect: DialectSQLite, Path: "sqlite/0001_base.sql", OwnerPhase: "P09", SHA256: "e7873b3f504595272913eb66b4656cbcb157ce901b1fd823bf9967ea64c5b348"},
			{Dialect: DialectSQLite, Path: "sqlite/0005_identity.sql", OwnerPhase: "P10", SHA256: "192ce590834794c68be040237a7611eed2631edf166d6c9fbd9930e63a1cf6ce"},
			{Dialect: DialectSQLite, Path: "sqlite/0010_publication.sql", OwnerPhase: "P12", SHA256: "524364c706ca6e336f5de0dd42058772699b7255f53cf099e96bef10e4973df8"},
			{Dialect: DialectSQLite, Path: "sqlite/0020_asset.sql", OwnerPhase: "P13", SHA256: "df73c99001ba32a6140dc67458b4033ce41bf797b7a08968d9ecf819e092747b"},
		},
	}
}
