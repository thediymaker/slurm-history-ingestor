@echo off
echo Installing sqlc...
go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest

echo Generating database code...
if not exist "internal\db" mkdir "internal\db"
%USERPROFILE%\go\bin\sqlc generate || sqlc generate

echo Initializing Go module...
go get github.com/ubccr/slurmrest@latest
go mod tidy

echo.
echo Setup complete! 
echo To run the ingestor:
echo set DATABASE_URL=postgres://user:pass@localhost:5432/slurm_db
echo set SLURM_SERVER=http://slurm:6820
echo go run cmd/ingest/main.go
pause
