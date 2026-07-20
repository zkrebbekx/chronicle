module github.com/zkrebbekx/chronicle/chronicled

go 1.25.0

require (
	github.com/jackc/pgx/v5 v5.9.2
	github.com/zkrebbekx/chronicle v0.1.0
	github.com/zkrebbekx/chronicle/pgstore v0.1.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/text v0.30.0 // indirect
)

replace github.com/zkrebbekx/chronicle => ../

replace github.com/zkrebbekx/chronicle/pgstore => ../pgstore
