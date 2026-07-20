module github.com/zkrebbekx/chronicle/pgstore

go 1.24.0

replace github.com/zkrebbekx/chronicle => ../

require (
	github.com/jackc/pgx/v5 v5.8.0
	github.com/zkrebbekx/chronicle v0.0.0-00010101000000-000000000000
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/text v0.30.0 // indirect
)
