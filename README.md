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

I'm using sqlite during development, but this should eventually move to
PostgreSQL.

### Search engine

Todo. We'll use Sphinx Search to search and rank packages.
