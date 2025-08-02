# pantry

Early thoughts on navigating a package ecosystem

## Components

### Scanner

The scanner ingests Go modules (as seen by index.golang.org) into a database.
It extracts documentation for each package in the module, and stores this so
that it can be used by the search engine.

Start the scanner with the following command:

```shell
go run cmd/scanner/scanner.go
```

### Server

The web server provides a searchable interface for the database of packages
found by the scanner.

Start the web server with the following command:

```shell
go run cmd/server/server.go
```

### Database

The database holds relevant information about all of the modules we know
about. The search engine indexes this information so that it can appropriately
rank search results.

The development database uses `docker compose` to run a local CockroachDB
instance. To start the database server, run:

```shell
docker compose up -d
```

To connect to the database console, run:

```shell
docker exec -it roach ./cockroach sql --insecure --database=pantry
```

### Search engine

The search engine processes user requests by identifying relevant packages and
ranking them.

The development search engine uses `docker compose` to run a local Manticore
search instance. To start the search engine, run:

```shell
docker compose up -d
```

To re-index the search database (for example, after scanning new packages into
the database), run:

```shell
docker exec -it --user manticore manticore indexer --all --rotate
```
