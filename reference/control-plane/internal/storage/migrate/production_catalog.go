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

// P13ProductionCatalog is the immutable historical catalog used when P13 is
// replayed under later phases. It must never include a migration owned after
// P13.
func P13ProductionCatalog() CatalogClosure {
	return CatalogClosure{
		ID: "control-plane-p13-production-migrations", ReportPhase: "P13", ReportPath: "build/reports/P13/report.json",
		OwnerPhases: []string{"P09", "P10", "P12", "P13"},
		Migrations: []DeclaredMigration{
			{Dialect: DialectPostgres, Path: "postgres/0001_base.sql", OwnerPhase: "P09", SHA256: "c9f04000d5ce26ee7d86d3b89131ac05861945f5e95537c52ffa6cf359388707"},
			{Dialect: DialectPostgres, Path: "postgres/0005_identity.sql", OwnerPhase: "P10", SHA256: "5d0211ac031d98dd3760701bc19dddd2d96d67cfc74dc5140db6b18ddda75dde"},
			{Dialect: DialectPostgres, Path: "postgres/0010_publication.sql", OwnerPhase: "P12", SHA256: "4d09c8ea3744a805c111d8c6942a6ee128aa374cc249cb818f0327ea13927a13"},
			{Dialect: DialectPostgres, Path: "postgres/0020_asset.sql", OwnerPhase: "P13", SHA256: "e6e57df2db19bb1fc95885de9ac39abb5389a239fd7ad139394832491a0411ed"},
			{Dialect: DialectSQLite, Path: "sqlite/0001_base.sql", OwnerPhase: "P09", SHA256: "e7873b3f504595272913eb66b4656cbcb157ce901b1fd823bf9967ea64c5b348"},
			{Dialect: DialectSQLite, Path: "sqlite/0005_identity.sql", OwnerPhase: "P10", SHA256: "192ce590834794c68be040237a7611eed2631edf166d6c9fbd9930e63a1cf6ce"},
			{Dialect: DialectSQLite, Path: "sqlite/0010_publication.sql", OwnerPhase: "P12", SHA256: "524364c706ca6e336f5de0dd42058772699b7255f53cf099e96bef10e4973df8"},
			{Dialect: DialectSQLite, Path: "sqlite/0020_asset.sql", OwnerPhase: "P13", SHA256: "069fce12fa53bebd186c01be6adb6466348dfd0a387e4bf9c2ad253c78c828b2"},
		},
	}
}

// P14ProductionCatalog is the immutable historical catalog used when P14-P17
// are replayed under later phases. It must never include a migration owned
// after P14.
func P14ProductionCatalog() CatalogClosure {
	return CatalogClosure{
		ID: "control-plane-p14-production-migrations", ReportPhase: "P14", ReportPath: "build/reports/P14/report.json",
		OwnerPhases: []string{"P09", "P10", "P12", "P13", "P14"},
		Migrations: []DeclaredMigration{
			{Dialect: DialectPostgres, Path: "postgres/0001_base.sql", OwnerPhase: "P09", SHA256: "c9f04000d5ce26ee7d86d3b89131ac05861945f5e95537c52ffa6cf359388707"},
			{Dialect: DialectPostgres, Path: "postgres/0005_identity.sql", OwnerPhase: "P10", SHA256: "5d0211ac031d98dd3760701bc19dddd2d96d67cfc74dc5140db6b18ddda75dde"},
			{Dialect: DialectPostgres, Path: "postgres/0010_publication.sql", OwnerPhase: "P12", SHA256: "4d09c8ea3744a805c111d8c6942a6ee128aa374cc249cb818f0327ea13927a13"},
			{Dialect: DialectPostgres, Path: "postgres/0020_asset.sql", OwnerPhase: "P13", SHA256: "e6e57df2db19bb1fc95885de9ac39abb5389a239fd7ad139394832491a0411ed"},
			{Dialect: DialectPostgres, Path: "postgres/0030_registry.sql", OwnerPhase: "P14", SHA256: "cb85370a58ef5cfabbf5205430c667cbda5d1a3cbfb2a7a6da4c2a6d7245ffe0"},
			{Dialect: DialectSQLite, Path: "sqlite/0001_base.sql", OwnerPhase: "P09", SHA256: "e7873b3f504595272913eb66b4656cbcb157ce901b1fd823bf9967ea64c5b348"},
			{Dialect: DialectSQLite, Path: "sqlite/0005_identity.sql", OwnerPhase: "P10", SHA256: "192ce590834794c68be040237a7611eed2631edf166d6c9fbd9930e63a1cf6ce"},
			{Dialect: DialectSQLite, Path: "sqlite/0010_publication.sql", OwnerPhase: "P12", SHA256: "524364c706ca6e336f5de0dd42058772699b7255f53cf099e96bef10e4973df8"},
			{Dialect: DialectSQLite, Path: "sqlite/0020_asset.sql", OwnerPhase: "P13", SHA256: "069fce12fa53bebd186c01be6adb6466348dfd0a387e4bf9c2ad253c78c828b2"},
			{Dialect: DialectSQLite, Path: "sqlite/0030_registry.sql", OwnerPhase: "P14", SHA256: "50d2d3c448062ab537e98b9399205bf02b39f9c5319995fa77df16b7a0d6f51d"},
		},
	}
}

// CurrentProductionCatalog is the only catalog used by production
// composition. P18 advances the paired history to 0040. Historical phases
// always inject their immutable snapshot instead.
func CurrentProductionCatalog() CatalogClosure {
	return CatalogClosure{
		ID: "control-plane-production-migrations", ReportPhase: "P18", ReportPath: "build/reports/P18/report.json",
		OwnerPhases: []string{"P09", "P10", "P12", "P13", "P14", "P18"},
		Migrations: []DeclaredMigration{
			{Dialect: DialectPostgres, Path: "postgres/0001_base.sql", OwnerPhase: "P09", SHA256: "c9f04000d5ce26ee7d86d3b89131ac05861945f5e95537c52ffa6cf359388707"},
			{Dialect: DialectPostgres, Path: "postgres/0005_identity.sql", OwnerPhase: "P10", SHA256: "5d0211ac031d98dd3760701bc19dddd2d96d67cfc74dc5140db6b18ddda75dde"},
			{Dialect: DialectPostgres, Path: "postgres/0010_publication.sql", OwnerPhase: "P12", SHA256: "4d09c8ea3744a805c111d8c6942a6ee128aa374cc249cb818f0327ea13927a13"},
			{Dialect: DialectPostgres, Path: "postgres/0020_asset.sql", OwnerPhase: "P13", SHA256: "e6e57df2db19bb1fc95885de9ac39abb5389a239fd7ad139394832491a0411ed"},
			{Dialect: DialectPostgres, Path: "postgres/0030_registry.sql", OwnerPhase: "P14", SHA256: "cb85370a58ef5cfabbf5205430c667cbda5d1a3cbfb2a7a6da4c2a6d7245ffe0"},
			{Dialect: DialectPostgres, Path: "postgres/0040_run.sql", OwnerPhase: "P18", SHA256: "3b93a3fea9694fad7f06db8ca6db22b8cca0b0a53f40ded7c4820809286a99cd"},
			{Dialect: DialectSQLite, Path: "sqlite/0001_base.sql", OwnerPhase: "P09", SHA256: "e7873b3f504595272913eb66b4656cbcb157ce901b1fd823bf9967ea64c5b348"},
			{Dialect: DialectSQLite, Path: "sqlite/0005_identity.sql", OwnerPhase: "P10", SHA256: "192ce590834794c68be040237a7611eed2631edf166d6c9fbd9930e63a1cf6ce"},
			{Dialect: DialectSQLite, Path: "sqlite/0010_publication.sql", OwnerPhase: "P12", SHA256: "524364c706ca6e336f5de0dd42058772699b7255f53cf099e96bef10e4973df8"},
			{Dialect: DialectSQLite, Path: "sqlite/0020_asset.sql", OwnerPhase: "P13", SHA256: "069fce12fa53bebd186c01be6adb6466348dfd0a387e4bf9c2ad253c78c828b2"},
			{Dialect: DialectSQLite, Path: "sqlite/0030_registry.sql", OwnerPhase: "P14", SHA256: "50d2d3c448062ab537e98b9399205bf02b39f9c5319995fa77df16b7a0d6f51d"},
			{Dialect: DialectSQLite, Path: "sqlite/0040_run.sql", OwnerPhase: "P18", SHA256: "e8fff4625a6e2fc57a43af359905deef60b1971c29e3453b995ba5d992b8e478"},
		},
	}
}
